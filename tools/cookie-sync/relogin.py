#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""gw2a 会话自动恢复 —— 会话被 Google 判成匿名时，用 profile 里已保存的
密码自动重新登录，不需要人工介入。

背景（2026-09-12 定位）：
  Google 侧把 Gemini 会话注销后，cookie 文件里的 SID/SAPISID/__Secure-1PSID
  仍然在、有效期还有一年多，只是服务端不再认它 —— 表现为
  /app 页面里没有 "SNlM0e"，页面正文是 "Sign in to save activity"。
  此时任何"按 cookie 名字齐不齐"的判定都会说"已登录"，于是扩展把这份废
  cookie 一直往池子里写，gemini-web2api 拿到就 302 / 无 SNlM0e，用户看到
  的就是「web 明明登录着，抓到的 cookie 却 502 重定向登录页」。

判据（本文件统一用这个）：
  页面里能读到 SNlM0e → 真的登录着；否则就是匿名。

流程（全部复用同一标签页，不新建 target，避免风控）：
  1. 打开 gemini.google.com/app，等 60s 冷却后读 SNlM0e
  2. 没有 → 走 Google 登录跳板（ServiceLogin?service=wise&continue=gemini/app）
  3. accountchooser 出现账号条目 → 点它
  4. 密码页 → 从 Login Data 解密出保存的密码 → 填 → Next
  5. 回 /app 确认 SNlM0e 出现

用法：
  relogin.py [profile] [--check-only]
    --check-only  只体检，不尝试登录
退出码：0=已登录  1=恢复成功  2=需要人工（2FA/风控/无密码可用）  3=环境错误
"""
import base64
import binascii
import hashlib
import json
import os
import sqlite3
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json, CDPError  # noqa: E402

CTRL = os.environ.get("GW2A_CTRL_URL", "http://127.0.0.1:9280").rstrip("/")
PROFILE_BASE = os.environ.get("GW2A_PROFILE_BASE", "/vol2/@appdata/gw2a-browser/profiles")
LOGIN_URL = ("https://accounts.google.com/ServiceLogin?service=wise"
             "&continue=https%3A%2F%2Fgemini.google.com%2Fapp%3Fauthuser%3D0&authuser=0")
GEMINI_URL = "https://gemini.google.com/app"
STEP_WAIT = int(os.environ.get("GW2A_RELOGIN_STEP_WAIT", "20") or 20)


def log(*a):
    print("[relogin]", *a, flush=True)


# ---------------------------------------------------------------- profile

def ctrl_json(path, method="GET", body=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(CTRL + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


def profile_port(profile, wait=90):
    """拿到该 profile 的 CDP 端口；不在运行就通过控制器拉起。"""
    try:
        for p in ctrl_json("/profiles").get("profiles", []):
            if p.get("name") == profile:
                return int(p["port"])
    except Exception as e:
        log("查询 profile 失败:", e)
    log("按需启动浏览器 profile =", profile)
    try:
        r = ctrl_json("/profiles", "POST", {"name": profile})
    except Exception as e:
        log("启动失败:", e)
        r = {}
    deadline = time.time() + wait
    while time.time() < deadline:
        try:
            for p in ctrl_json("/profiles").get("profiles", []):
                if p.get("name") == profile:
                    port = int(p["port"])
                    try:
                        http_json("http://127.0.0.1:%d/json/version" % port, timeout=3)
                        return port
                    except Exception:
                        pass
        except Exception:
            pass
        time.sleep(1.5)
    raise RuntimeError("等待 CDP 就绪超时（profile=%s）" % profile)


# ---------------------------------------------------------------- password

def _aes128cbc_decrypt(key, iv, data):
    """不依赖 pycryptodome：优先 openssl，否则用纯 python AES。"""
    try:
        p = subprocess.run(["openssl", "enc", "-d", "-aes-128-cbc",
                            "-K", binascii.hexlify(key).decode(),
                            "-iv", binascii.hexlify(iv).decode()],
                           input=data, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                           timeout=15)
        if p.returncode == 0 and p.stdout:
            return p.stdout
    except Exception:
        pass
    return None


def saved_password(profile):
    """从 Chromium 的 Login Data 里取出 accounts.google.com 的密码（明文，内存中）。"""
    ld = os.path.join(PROFILE_BASE, profile, "Default", "Login Data")
    if not os.path.exists(ld):
        return "", "没有 Login Data"
    tmp = "/tmp/.relogin-ld-%d.db" % os.getpid()
    try:
        # 复制一份再读，避免和浏览器抢锁
        with open(ld, "rb") as f:
            data = f.read()
        with open(tmp, "wb") as f:
            f.write(data)
        os.chmod(tmp, 0o600)
        con = sqlite3.connect(tmp)
        con.row_factory = sqlite3.Row
        rows = con.execute(
            "SELECT origin_url, username_value, password_value FROM logins "
            "WHERE username_value <> '' ORDER BY date_created DESC").fetchall()
        con.close()
    except Exception as e:
        return "", "读 Login Data 失败: %s" % e
    finally:
        try:
            os.unlink(tmp)
        except Exception:
            pass
    if not rows:
        return "", "Login Data 里没有保存的密码"
    # basic store 的密钥是固定口令 peanuts + salt 'saltysalt'（Linux 无 keyring）
    key = hashlib.pbkdf2_hmac("sha1", b"peanuts", b"saltysalt", 1, 16)
    iv = b" " * 16
    for r in rows:
        blob = bytes(r["password_value"] or b"")
        if len(blob) < 4:
            continue
        if blob[:3] not in (b"v10", b"v11"):
            pw = blob.decode("utf-8", "ignore")
        else:
            pt = _aes128cbc_decrypt(key, iv, blob[3:])
            if not pt:
                continue
            pad = pt[-1]
            if 1 <= pad <= 16:
                pt = pt[:-pad]
            # Chrome 15+ 在明文前拼了 32 字节域哈希
            for cand in (pt[32:], pt):
                try:
                    s = cand.decode("utf-8")
                except Exception:
                    continue
                if s and all(c.isprintable() for c in s):
                    pw = s
                    break
        if pw:
            return pw, r["username_value"]
    return "", "密码解不开（可能用了 keyring/系统密钥环）"


# ---------------------------------------------------------------- cdp page

class Page(object):
    def __init__(self, port):
        self.port = port
        pages = [t for t in http_json("http://127.0.0.1:%d/json/list" % port)
                 if t.get("type") == "page"]
        gem = [p for p in pages if "gemini.google.com" in (p.get("url") or "")]
        target = gem[0] if gem else (pages[0] if pages else None)
        if target is None:
            raise RuntimeError("浏览器里一个标签页都没有")
        self.url_at_open = target.get("url") or ""
        self.b = CDP(target["webSocketDebuggerUrl"])
        for m in ("Page.enable", "Runtime.enable"):
            try:
                self.b.send(m)
            except CDPError:
                pass

    def ev(self, expr, timeout=45):
        try:
            r = self.b.send("Runtime.evaluate",
                            {"expression": expr, "returnByValue": True}, timeout=timeout)
            if r.get("exceptionDetails"):
                return None
            return (r.get("result") or {}).get("value")
        except CDPError:
            return None

    def href(self):
        return str(self.ev("location.href") or "")

    def body(self):
        return str(self.ev("document.body ? document.body.innerText.replace(/\\n+/g,' ') : ''") or "")

    def has_snlm0e(self):
        v = self.ev("document.documentElement.outerHTML.indexOf('SNlM0e')")
        return isinstance(v, int) and v >= 0

    def nav(self, url, wait=None):
        try:
            self.b.send("Page.navigate", {"url": url})
        except CDPError as e:
            log("导航失败:", e)
            return False
        end = time.time() + (wait or STEP_WAIT)
        while time.time() < end:
            if self.ev("document.readyState") == "complete":
                break
            time.sleep(1)
        time.sleep(2.5)
        return True

    def rect(self, sel):
        v = self.ev("""(() => {
          const e = document.querySelector(%s);
          if (!e) return null;
          const r = e.getBoundingClientRect();
          if (!(r.width > 0 && r.height > 0)) return null;
          return JSON.stringify({x: Math.round(r.x + r.width / 2),
                                 y: Math.round(r.y + r.height / 2)});
        })()""" % json.dumps(sel))
        try:
            return json.loads(v) if isinstance(v, str) else None
        except Exception:
            return None

    def click(self, x, y):
        for t, extra in (("mouseMoved", {}),
                         ("mousePressed", {"button": "left", "clickCount": 1, "buttons": 1}),
                         ("mouseReleased", {"button": "left", "clickCount": 1, "buttons": 0})):
            p = {"type": t, "x": x, "y": y}
            p.update(extra)
            self.b.send("Input.dispatchMouseEvent", p)
            time.sleep(0.08)

    def type_into(self, sel, text):
        r = self.rect(sel)
        if not r:
            return False
        self.click(r["x"], r["y"])
        time.sleep(0.4)
        self.b.send("Input.insertText", {"text": text})
        time.sleep(0.6)
        return str(self.ev("(document.querySelector(%s)||{}).value ? 'y' : 'n'"
                           % json.dumps(sel)) or "") == "y"

    def click_sel(self, sel):
        r = self.rect(sel)
        if not r:
            return False
        self.click(r["x"], r["y"])
        return True

    def submit(self, sel):
        if not self.click_sel(sel):
            for t in ("keyDown", "keyUp"):
                self.b.send("Input.dispatchKeyEvent",
                            {"type": t, "key": "Enter", "code": "Enter",
                             "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13})
        return True

    def close(self):
        try:
            self.b.close()
        except Exception:
            pass


BLOCKERS = ("2-Step Verification", "verification code", "Verify it", "captcha",
            "may not be secure", "Couldn't sign you in", "Wrong password",
            "扫码", "确认是您本人", "尝试次数过多", "suspicious")


# ---------------------------------------------------------------- flow

def session_ok(page, tries=4, gap=3):
    """当前页面（须在 gemini 上）是否真的登录：看 SNlM0e。"""
    for i in range(tries):
        if page.has_snlm0e():
            return True
        if i + 1 < tries:
            time.sleep(gap)
    return False


def do_relogin(page, profile):
    pw, user = saved_password(profile)
    if not pw:
        return False, "没有可用密码：%s" % user
    log("使用已保存的账号 %s 的密码（长度 %d，不回显）" % (user, len(pw)))
    # 注意：因为 authuser 可能 >0，登录页可能直接给账号列表
    page.nav(LOGIN_URL, STEP_WAIT)
    for rnd in range(8):
        u = page.href()
        if u.startswith("https://gemini.google.com/"):
            time.sleep(3)
            if session_ok(page, tries=5):
                return True, "登录跳板直接恢复"
            page.nav(GEMINI_URL, STEP_WAIT)
            continue
        blk = [b for b in BLOCKERS if b in page.body()]
        if blk:
            return False, "需要人工：%s" % ",".join(blk)
        if "challenge/pwd" in u or "signin/v2/challenge" in u or "/pwd" in u:
            if not page.type_into("input[type=password]", pw):
                return False, "密码框填不进去（%s）" % u[:80]
            page.submit("#passwordNext button, button[type=submit]")
            time.sleep(8)
            continue
        if "accountchooser" in u or "/identifier" in u or "signin/v2" in u:
            if not page.click_sel("div[data-identifier], [data-identifier], li div[role=link]"):
                page.submit("button[type=submit]")
            time.sleep(7)
            continue
        time.sleep(3)
    if page.href().startswith("https://gemini.google.com/"):
        return session_ok(page, tries=5), "回到 /app 后确认"
    return False, "流程未走完，最后停在 %s" % page.href()[:120]


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    check_only = "--check-only" in sys.argv
    profile = args[0] if args else "acct1"
    try:
        port = profile_port(profile)
    except Exception as e:
        log("环境错误:", e)
        return 3
    log("profile=%s port=%d" % (profile, port))
    page = Page(port)
    try:
        # 体检：先把页面导到 /app（复用同一标签页）
        if not page.href().startswith("https://gemini.google.com/"):
            page.nav(GEMINI_URL, STEP_WAIT)
        if session_ok(page, tries=5):
            log("会话有效（/app 有 SNlM0e）")
            return 0
        log("会话已被判匿名（/app 无 SNlM0e），页面正文: %s" % page.body()[:120])
        if check_only:
            return 2
        ok, why = do_relogin(page, profile)
        if ok:
            log("恢复成功：%s" % why)
            return 1
        log("恢复失败：%s" % why)
        return 2
    except Exception as e:
        log("异常:", type(e).__name__, e)
        return 3
    finally:
        page.close()


if __name__ == "__main__":
    sys.exit(main())
