#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""gw2a-cookie-refresh —— 按需启动浏览器抓 cookie（用完即关，避免常驻占 CPU）。

★ 风控规避（v1.0.5 起）★
  不再「每次抓取都新建一个 Gemini 页面」：同一个账号下短时间内反复产生新会话
  会被 Google 风控（会话被踢 / sorry 页）。现在改为：

    1. 复用已有页面 —— 依次找：① 已打开的 gemini.google.com 页面；
       ② 被风控重定向后的 google.com/sorry 页面（它就是这个账号的 Gemini 页，
       旧逻辑认不出来才会去新建）；③ 浏览器启动时的 about:blank 等任意页面。
       只有在完全没有页面时才创建 1 个 target。
    2. 刷新（Page.reload / Page.navigate）后进入 120 秒冷却窗口
       （GW2A_COOLDOWN_SEC，默认 120），窗口内每 5 秒提取一次 cookie，绝不重复导航。
    3. 60 秒窗口结束仍未取到可用 cookie，才允许再刷新一次
       （最多 GW2A_MAX_REFRESH 次，默认 3 次），避免死循环刷页面。
    4. 整个流程最多占用 GW2A_BUDGET_SEC（默认 240 秒），超时直接放弃本轮。
    5. 每次刷新后把时间戳写进 /var/lib/gw2a-cookie-sync/refresh-<profile>.json，
       供下一轮判断「是否还在冷却窗口内」。

流程：
  1. 确保 profile 的 Chromium 在跑（不在就通过控制器拉起，等 CDP 就绪）
  2. 刷新（或首次导航）Gemini 页面，等加载完成
  3. 在 120s 冷却窗口内轮询提取 cookie
  4. 写池（pool.py upsert：更新同 profile 记录 + 删除同 profile 旧记录）
     并绑定出口代理（与浏览器同一出口，避免 IP 不一致被判可疑）
  5. 调容器 /check 校验；失败则记录（本地限流满时跳过，不算失败）
  6. 关闭该 profile 的浏览器，释放 CPU

用法：
  refresh.py [profile] [--force-refresh] [--wait-cooldown] [--stop]
  （默认不关浏览器；--stop 才关）
"""
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json  # noqa: E402

CTRL = os.environ.get("GW2A_CTRL_URL", "http://127.0.0.1:9280").rstrip("/")
API = os.environ.get("GW2A_API", "http://127.0.0.1:8083").rstrip("/")
ADMIN_TOKEN = os.environ.get("GW2A_ADMIN_TOKEN", "")
POOL_PY = os.environ.get("GW2A_POOL_PY", "/opt/gw2a-cookie-sync/pool.py")
PY = os.environ.get("GW2A_PYTHON", "/usr/bin/python3")
BROWSER_PROXY = os.environ.get("GW2A_BROWSER_PROXY", "")
PROXY_ID = int(os.environ.get("GW2A_BROWSER_PROXY_ID", "0") or 0)
GEMINI_URL = "https://gemini.google.com/app"
PROFILE = "acct1"

COOLDOWN_SEC = int(os.environ.get("GW2A_COOLDOWN_SEC", "120") or 120)  # 刷新后的冷却窗口
MAX_REFRESH = int(os.environ.get("GW2A_MAX_REFRESH", "3") or 3)       # 一轮最多刷新几次
BUDGET_SEC = int(os.environ.get("GW2A_BUDGET_SEC", "240") or 240)     # 一轮总时长上限
POLL_SEC = float(os.environ.get("GW2A_POLL_SEC", "5") or 5)
STATE_DIR = os.environ.get("GW2A_STATE_DIR", "/var/lib/gw2a-cookie-sync")
# 强制刷新（502 触发）时也要保留的最小间隔：避免「刚刷新完又立刻刷」
# 这种反风控节奏。冷却窗口优先，但强制刷新不得比这个更密。
MIN_GAP_SEC = int(os.environ.get("GW2A_MIN_GAP_SEC", "30") or 30)

COOKIE_ORDER = [
    "SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID", "APISID",
    "SID", "HSID", "SSID", "__Secure-1PSID", "__Secure-3PSID",
    "__Secure-1PSIDTS", "__Secure-3PSIDTS",
    "__Secure-1PSIDCC", "__Secure-3PSIDCC", "SIDCC",
    "LSID", "__Host-1PLSID", "__Host-3PLSID", "ACCOUNT_CHOOSER",
    "NID", "COMPASS", "__Secure-ENID", "__Secure-1PSIDRTS", "__Secure-3PSIDRTS",
]
AUTH_ANY = [["SAPISID", "SID", "__Secure-1PSID"], ["__Secure-1PSID", "__Secure-3PSID"]]
SKIP_RE = ("_ga", "_gcl", "_gid", "OTZ", "OSID", "GOOGLE_ABUSE_EXEMPTION")


def log(*a):
    print("[refresh]", *a, flush=True)


# ---------------------------------------------------------------- state

def state_path(profile):
    return os.path.join(STATE_DIR, "refresh-%s.json" % profile)


def load_state(profile):
    try:
        with open(state_path(profile), "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return {}


def save_state(profile, **kw):
    st = load_state(profile)
    st.update(kw)
    try:
        os.makedirs(STATE_DIR, exist_ok=True)
        tmp = state_path(profile) + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(st, f, ensure_ascii=False)
        os.replace(tmp, state_path(profile))
    except Exception as e:
        log("写状态失败（忽略）:", e)
    return st


# ---------------------------------------------------------------- http

def ctrl_json(path, method="GET", body=None, timeout=120):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        CTRL + path, data=data, method=method,
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


def api_json(path, method="GET", body=None, jar=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"}
    if jar:
        headers["Cookie"] = jar
    req = urllib.request.Request(API + path, data=data, method=method, headers=headers)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8", "replace")), r.headers


def admin_login():
    if not ADMIN_TOKEN:
        return None
    try:
        r, h = api_json("/admin/api/login", "POST", {"token": ADMIN_TOKEN})
        if not r.get("ok"):
            return None
        sc = h.get_all("Set-Cookie") or []
        return "; ".join(c.split(";")[0] for c in sc) or None
    except Exception as e:
        log("admin 登录失败:", e)
        return None


def resolve_proxy_id():
    if PROXY_ID > 0:
        return PROXY_ID
    if not BROWSER_PROXY:
        return 0
    jar = admin_login()
    if not jar:
        return 0
    try:
        r, _ = api_json("/admin/api/proxies", jar=jar)
        want = BROWSER_PROXY.rstrip("/")
        for it in (r.get("items") or []):
            if str(it.get("url") or "").rstrip("/") == want:
                return int(it["id"])
    except Exception as e:
        log("查询代理表失败:", e)
    return 0


def run_pool(sub, obj):
    try:
        p = subprocess.run([PY, POOL_PY, sub], input=json.dumps(obj).encode(),
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
        out = p.stdout.decode("utf-8", "replace").strip()
        return json.loads(out) if out else {"error": "empty output"}
    except Exception as e:
        return {"error": "%s: %s" % (type(e).__name__, e)}


# ---------------------------------------------------------------- browser

def running_port(profile):
    try:
        for p in ctrl_json("/profiles").get("profiles", []):
            if p.get("name") == profile:
                return int(p["port"])
    except Exception as e:
        log("查询 profile 失败:", e)
    return None


def ensure_browser(profile, wait=75):
    port = running_port(profile)
    if port:
        log("浏览器已在运行，端口", port)
        return port, False
    log("按需启动浏览器 profile =", profile)
    try:
        r = ctrl_json("/profiles", "POST", {"name": profile})
    except urllib.error.HTTPError as e:
        log("启动请求 HTTP", e.code)
        r = {}
    except Exception as e:
        log("启动失败:", e)
        r = {}
    port = r.get("port")
    deadline = time.time() + wait
    while time.time() < deadline:
        p = running_port(profile)
        if p:
            try:
                http_json("http://127.0.0.1:%d/json/version" % p, timeout=3)
                log("CDP 就绪，端口", p)
                return p, True
            except Exception:
                pass
        time.sleep(1.5)
    raise RuntimeError("等待 CDP 就绪超时")


def stop_browser(profile):
    try:
        ctrl_json("/profiles/%s/stop" % profile, "POST", {}, timeout=60)
        log("已关闭浏览器", profile)
    except Exception as e:
        log("关闭浏览器失败（忽略）:", e)


# 会话探针结果：{"snlm0e": True/False/None, "checked_at": ts}
SESSION_STATE = {}


def probe_session(port):
    """在 gemini 页面里探 SNlM0e，判断会话是否真的可用。

    返回 True/False；探不到（没页面/超时）返回 None，此时 auth_ok 退回名字判定。
    """
    try:
        b = CDP(http_json("http://127.0.0.1:%d/json/version" % port)["webSocketDebuggerUrl"])
    except Exception:
        return None
    sid = None
    try:
        pages = [t for t in b.send("Target.getTargets")["targetInfos"]
                 if t.get("type") == "page"]
        gem = [t for t in pages if "gemini.google.com" in (t.get("url") or "")]
        if not gem:
            return None
        sid = b.send("Target.attachToTarget",
                     {"targetId": gem[0]["targetId"], "flatten": True})["sessionId"]
        b.send("Runtime.enable", session_id=sid)
        r = b.send("Runtime.evaluate", {
            "expression": ("document.documentElement.outerHTML"
                           ".indexOf('SNlM0e') >= 0 ? '1' : '0'"),
            "returnByValue": True}, session_id=sid, timeout=15)
        v = (r.get("result") or {}).get("value")
        ok = (v == "1")
        SESSION_STATE["snlm0e"] = ok
        SESSION_STATE["checked_at"] = time.time()
        return ok
    except Exception:
        return None
    finally:
        try:
            b.close()
        except Exception:
            pass


def read_cookies(port):
    b = CDP(http_json("http://127.0.0.1:%d/json/version" % port)["webSocketDebuggerUrl"])
    try:
        cks = (b.send("Storage.getCookies", timeout=60).get("cookies") or [])
    finally:
        b.close()
    have = {}
    for c in cks:
        host = c.get("domain") or ""
        name = c.get("name") or ""
        val = c.get("value") or ""
        if "google" not in host or not val or not name:
            continue
        if name.startswith(SKIP_RE):
            continue
        prev = have.get(name)
        if prev is None or (host == ".google.com" and prev[0] != ".google.com"):
            have[name] = (host, val)
    return have


def auth_ok(have):
    """cookie 名字齐 = 登录态？不一定。

    实测（2026-09-12）：SID/SAPISID/__Secure-1PSID 全在、有效期到 2027，
    但 Gemini 服务端已把这份会话判成匿名（/app 页面里没有 SNlM0e），此时按
    名字判定会说「已登录」，抓下来的 cookie 写进池子全是 502。

    所以名字只作必要条件；真正的判据是页面里有没有 SNlM0e —— 由
    probe_session() 探测，结果放 SESSION_STATE 传进来。没探过时退回名字判定，
    保持向后兼容，避免漏抓。
    """
    named = False
    for grp in AUTH_ANY:
        for k in grp:
            if have.get(k):
                named = True
                break
        if named:
            break
    if not named:
        return False
    st = SESSION_STATE.get("snlm0e")
    if st is None:
        return True
    return bool(st)


def refresh_page(port, url=GEMINI_URL, wait_sec=45):
    """刷新（或首次导航）Gemini 页面。绝不无脑新建 target。

    选页优先级：
      ① 已打开的 gemini.google.com 页面
      ② 被 Google 风控重定向后的 google.com/sorry 页面
         （它就是这个账号的 Gemini 页；旧逻辑认不出来才会去新建，风控就是这么来的）
      ③ 浏览器启动时的 about:blank / newtab 等任意页面
      ④ 真的一个页面都没有，才 createTarget

    返回 dict：{ok, mode, closed, url, sorry}
    """
    b = CDP(http_json("http://127.0.0.1:%d/json/version" % port)["webSocketDebuggerUrl"])
    out = {"ok": False, "mode": "", "closed": 0, "url": "", "sorry": False}
    sid = None
    try:
        pages = [t for t in b.send("Target.getTargets")["targetInfos"]
                 if t.get("type") == "page"]

        def is_sorry(t):
            return "/sorry/" in (t.get("url") or "")

        gem = [t for t in pages if "gemini.google.com" in (t.get("url") or "")]
        sorry = [t for t in pages if is_sorry(t)]
        if gem:
            target, mode = gem[0], "reused"
        elif sorry:
            target, mode = sorry[0], "reused-sorry"
        elif pages:
            target, mode = pages[0], "reused-blank"
        else:
            tid = b.send("Target.createTarget",
                         {"url": url, "background": True})["targetId"]
            target, mode = {"targetId": tid}, "created"

        tid = target["targetId"]
        sid = b.send("Target.attachToTarget",
                     {"targetId": tid, "flatten": True})["sessionId"]
        b.send("Page.enable", session_id=sid)
        b.send("Runtime.enable", session_id=sid)

        cur = ""
        try:
            r = b.send("Runtime.evaluate",
                       {"expression": "location.href", "returnByValue": True},
                       session_id=sid, timeout=15)
            cur = (r.get("result") or {}).get("value") or ""
        except Exception:
            pass

        if mode == "created":
            pass  # createTarget 已经在导航
        elif "gemini.google.com" in cur:
            b.send("Page.reload", {"ignoreCache": False}, session_id=sid)
        else:
            b.send("Page.navigate", {"url": url}, session_id=sid)

        deadline = time.time() + wait_sec
        while time.time() < deadline:
            try:
                r = b.send("Runtime.evaluate",
                           {"expression": "location.href + '|' + document.readyState",
                            "returnByValue": True}, session_id=sid, timeout=15)
                v = (r.get("result") or {}).get("value") or ""
            except Exception:
                v = ""
            if v.startswith("https://gemini.google.com/") and v.endswith("complete"):
                break
            time.sleep(1)

        # 关掉多余的 gemini 页面（只留正在用的这个）；sorry 页永远不关 ——
        # 关掉它下次就找不到可复用的页面，又会新建，正是风控来源。
        # 清理旧页面：只保留正在用的那一个（tid），其余 gemini / sorry 页全部关掉 ——
        # 用户要求「旧的同步删除」；而历史遗留的一堆 sorry 页本身也会被 Google
        # 视为异常行为。保留 tid 就够下次复用了（它自己可能就是 sorry 页）。
        # 待关闭集合 = 所有 gemini 页 + 所有 sorry 页，去重后排除正在用的 tid
        stale = {}
        for t in gem + sorry:
            stale[t["targetId"]] = t
        stale.pop(tid, None)
        closed = 0
        for t in stale.values():
            try:
                b.send("Target.closeTarget", {"targetId": t["targetId"]})
                closed += 1
            except Exception:
                pass
        final = ""
        if closed:
            log("已清理历史遗留页面 %d 个（只保留正在用的 1 个）" % closed)
        try:
            r = b.send("Runtime.evaluate",
                       {"expression": "location.href", "returnByValue": True},
                       session_id=sid, timeout=15)
            final = (r.get("result") or {}).get("value") or ""
        except Exception:
            pass
        out.update({"ok": True, "mode": mode, "closed": closed,
                    "url": final, "sorry": "/sorry/" in final})
        if out["sorry"]:
            log("⚠ 页面被 Google 风控重定向到 sorry 页（%s），本轮只提取 cookie，不再反复刷新" % final[:80])
        log("页面已刷新（%s），关闭多余页 %d 个" % (mode, closed))
    except Exception as e:
        out["error"] = "%s: %s" % (type(e).__name__, e)
        log("刷新页面失败:", out["error"])
    finally:
        b.close()
    return out


# ---------------------------------------------------------------- main

def revive_session(port):
    """会话被 Google 判为匿名时，走一次 Google 登录跳板把它救回来。

    背景（实测 2026-09-12）：GAIA 层（accounts.google.com）的登录态是好的
    —— AccountChooser 能看到邮箱；但 Gemini 自己那份会话会被服务端踢掉，
    表现为 /app 页面里没有 "SNlM0e"（请求会被当匿名处理），而 cookie 文件里
    SID/SAPISID/__Secure-1PSID 全在、有效期还有一年多。此时只看 cookie 名字
    会误判成「已登录」，抓下来的 cookie 却是无效的。

    救法：用同一个 profile 访问一次 accounts.google.com 的登录跳板
    （ServiceLogin + continue 指回 gemini），Google 会用还活着的 GAIA 会话
    静默重新下发 Gemini 的会话 cookie。实测走完这一步 SNlM0e 就回来了。

    返回 True 表示救活（页面里能读到 SNlM0e）。
    """
    url = ("https://accounts.google.com/ServiceLogin?service=wise"
           "&continue=https%3A%2F%2Fgemini.google.com%2Fapp%3Fauthuser%3D0"
           "&authuser=0")
    b = CDP(http_json("http://127.0.0.1:%d/json/version" % port)["webSocketDebuggerUrl"])
    sid = None
    try:
        pages = [t for t in b.send("Target.getTargets")["targetInfos"]
                 if t.get("type") == "page"]
        gem = [t for t in pages if "gemini.google.com" in (t.get("url") or "")]
        if gem:
            tid = gem[0]["targetId"]
        else:
            tid = b.send("Target.createTarget",
                         {"url": url, "background": True})["targetId"]
        sid = b.send("Target.attachToTarget",
                     {"targetId": tid, "flatten": True})["sessionId"]
        b.send("Page.enable", session_id=sid)
        b.send("Runtime.enable", session_id=sid)
        expr = ("document.documentElement.outerHTML"
                ".indexOf('SNlM0e') >= 0 ? '1' : '0'")

        def has_snlm():
            try:
                r = b.send("Runtime.evaluate",
                           {"expression": expr, "returnByValue": True},
                           session_id=sid, timeout=15)
                return (r.get("result") or {}).get("value") == "1"
            except Exception:
                return False

        # 步骤 1：走 Google 登录跳板。GAIA 还活着时，Google 会在这里静默
        # 重新下发 Gemini 的会话 cookie（页面会带 SNlM0e）。
        log("会话恢复：步骤1 走 Google 登录跳板")
        b.send("Page.navigate", {"url": url}, session_id=sid)
        deadline = time.time() + 40
        bounced = False
        while time.time() < deadline:
            time.sleep(3)
            if has_snlm():
                bounced = True
                break
        if not bounced:
            log("会话恢复：跳板页未出现 SNlM0e")
            return False

        # 步骤 2：回到 gemini /app。会话 cookie 这时才真正落到 gemini 域上，
        # 必须回来看一次才算恢复完成（实测：不回就还是匿名）。
        log("会话恢复：步骤2 回 gemini /app 确认")
        b.send("Page.navigate", {"url": "https://gemini.google.com/app"}, session_id=sid)
        deadline = time.time() + 40
        while time.time() < deadline:
            time.sleep(3)
            if has_snlm():
                log("会话恢复：已恢复（/app 拿到 SNlM0e）")
                time.sleep(3)   # 让 Set-Cookie 落进 cookie store
                return True
        log("会话恢复：回到 /app 后仍未拿到 SNlM0e")
        return False
    except Exception as e:
        log("会话恢复失败（忽略）:", e)
        return False
    finally:
        try:
            b.close()
        except Exception:
            pass


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    # v1.1.0（2026-09-12）默认不再关闭浏览器。
    # 实测：浏览器一关，Google 在 gemini 页面里挂的 RotateCookiesPage iframe
    # 就没了跑完 __Secure-1PSIDTS 轮转的机会；SIGKILL 更会让 cookie 来不及
    # 落盘。轮转停摆后服务端会把会话判成匿名（页面无 SNlM0e），抓下来的
    # cookie 全是废的——这正是「登录缓存每次被清除」的真正来源。
    # 因此：常驻浏览器 + 扩展定时轮转才是正确姿势，只有显式 --stop 才关闭。
    stop_after = "--stop" in sys.argv
    keep_open = "--keep-open" in sys.argv
    no_stop = "--no-stop" in sys.argv
    force = "--force-refresh" in sys.argv
    # 队列模式：不管冷却剩多久，都等到窗口结束再决定是否刷新；
    # 若窗口内已能取到 cookie 就直接用，否则到点后刷新一次。
    wait_cooldown = "--wait-cooldown" in sys.argv
    profile = args[0] if args else PROFILE
    t0 = time.time()
    out = {"profile": profile, "ok": False, "ts": int(t0),
           "cooldown_sec": COOLDOWN_SEC, "max_refresh": MAX_REFRESH}

    port, launched = ensure_browser(profile)
    out["port"] = port
    out["launched"] = launched
    refreshes = 0
    have = {}
    try:
        # ── 刷新 + 60s 冷却窗口内提取 ───────────────────────────────────
        while True:
            st = load_state(profile)
            since = time.time() - float(st.get("lastRefreshAt") or 0)
            do_refresh = force or since >= COOLDOWN_SEC
            if wait_cooldown and not force and since < COOLDOWN_SEC:
                # 队列触发：先看当前 cookie 是否可用，可用就免刷新直接入池
                probe_session(port)
                have = read_cookies(port)
                if auth_ok(have):
                    log("冷却中但当前 cookie 可用（剩 %.0fs），不刷新" % (COOLDOWN_SEC - since))
                    out["cookie_count"] = len(have)
                    out["logged_in"] = True
                    break
                # 不可用 → 等到冷却结束再刷新（不违反风控节奏）
                wait = COOLDOWN_SEC - since
                log("冷却中且 cookie 不可用，等待 %.0fs 后刷新" % wait)
                time.sleep(min(wait, max(0.0, BUDGET_SEC - (time.time() - t0))))
                force = True
                continue
            # 强制刷新（502 触发）也保留最小间隔，防止连环刷新触发风控
            if force and 0 < since < MIN_GAP_SEC:
                wait = MIN_GAP_SEC - since
                log("距上次刷新仅 %.0fs（<最小间隔 %ds），等 %.0fs 再刷" %
                    (since, MIN_GAP_SEC, wait))
                time.sleep(wait)
            if do_refresh and refreshes < MAX_REFRESH and (time.time() - t0) < BUDGET_SEC:
                r = refresh_page(port)
                out.setdefault("refreshes", []).append(r)
                if r.get("ok"):
                    refreshes += 1
                    probe_session(port)   # 刷新后立刻探会话真假
                    st = save_state(profile, lastRefreshAt=time.time(),
                                    lastRefreshMode=r.get("mode"),
                                    lastRefreshUrl=r.get("url"),
                                    sorry=bool(r.get("sorry")))
                    since = 0.0
                else:
                    time.sleep(5)   # 刷新失败（页面刚崩等）→ 稍等再试，避免空转
            window_end = float(st.get("lastRefreshAt") or 0) + COOLDOWN_SEC
            log("冷却窗口提取中（已刷新 %d 次，窗口剩 %.0fs）"
                % (refreshes, max(0.0, window_end - time.time())))
            while time.time() < window_end and (time.time() - t0) < BUDGET_SEC:
                # 先探会话真假（SNlM0e），再决定 cookie 能不能用 —— 名字齐不等于
                # 会话活着，只看名字会把失效会话写进池子。
                probe_session(port)
                have = read_cookies(port)
                if auth_ok(have):
                    break
                # 探针说会话已死 → 立刻走登录跳板救（每轮最多一次），
                # 救回来就直接提取，不用等冷却窗口耗尽。
                if SESSION_STATE.get("snlm0e") is False and not out.get("revived"):
                    out["revived"] = True
                    if revive_session(port):
                        out["revive_ok"] = True
                        time.sleep(3)
                        probe_session(port)
                        have = read_cookies(port)
                        if auth_ok(have):
                            out["cookie_count"] = len(have)
                            out["logged_in"] = True
                            break
                time.sleep(POLL_SEC)
            out["cookie_count"] = len(have)
            out["logged_in"] = auth_ok(have)
            if out["logged_in"]:
                break
            if refreshes >= MAX_REFRESH:
                log("已刷新 %d 次仍无可用 cookie，放弃本轮" % refreshes)
                break
            if (time.time() - t0) >= BUDGET_SEC:
                log("超出本轮时间预算 %ds，放弃" % BUDGET_SEC)
                break
            log("冷却窗口结束仍未取到有用 cookie，再次刷新页面")
            force = True
        out["refreshes_count"] = refreshes

        if not have:
            out["error"] = "没读到任何 google cookie"
            return 1
        if not auth_ok(have):
            out["error"] = "未登录（缺少 SID/SAPISID/__Secure-1PSID）"
            return 2
        # 名字齐但探针说会话已死 → 走登录跳板救一次，救不活就别把废 cookie 写进池子
        if SESSION_STATE.get("snlm0e") is False and not out.get("revive_ok"):
            out["revived"] = True
            if revive_session(port):
                out["revive_ok"] = True
                time.sleep(3)
                have = read_cookies(port)
                probe_session(port)
            if SESSION_STATE.get("snlm0e") is False:
                out["error"] = ("会话已被 Google 判为匿名（页面无 SNlM0e），"
                                "登录跳板也没救回来；请在浏览器里重新登录一次 Gemini")
                out["ok"] = False
                out["cookie_len"] = 0
                return 5

        # ── 组 cookie 头 ──────────────────────────────────────────────
        parts, names = [], []
        for n in COOKIE_ORDER:
            if n in have:
                parts.append(n + "=" + have[n][1])
                names.append(n)
        for n, (host, v) in have.items():
            if n in names:
                continue
            parts.append(n + "=" + v)
            names.append(n)
        cookie = "; ".join(parts)
        out["cookie_len"] = len(cookie)

        # ── 入池 ─────────────────────────────────────────────────────
        up = run_pool("upsert", {"profile": profile, "source": "browser",
                                 "label": "browser:" + profile,
                                 "note": "on-demand refresh x%d @ %s"
                                         % (refreshes, time.strftime("%F %T")),
                                 "cookie": cookie})
        out["upsert"] = up
        if up.get("error"):
            out["error"] = "入池失败: " + up["error"]
            return 3
        aid = up.get("id")
        out["id"] = aid

        pid = resolve_proxy_id()
        if pid > 0 and aid:
            out["bind"] = run_pool("set_proxy", {"id": aid, "proxy_id": pid})
            out["proxy_id"] = pid

        # ── 校验（会打一次上游；本地限流满时跳过，不算失败）─────────────
        jar = admin_login()
        if jar and aid:
            try:
                ck, _ = api_json("/admin/api/cookies/%d/check" % aid, "POST", {}, jar=jar)
                out["check"] = ck
                if ck.get("ok"):
                    run_pool("set_ok", {"id": aid})
                else:
                    d = str(ck.get("detail") or "")
                    if "rph limit" in d or "slots full" in d:
                        out["check_skipped"] = d
                    else:
                        run_pool("set_fail", {"id": aid, "error": "check: " + d})
            except Exception as e:
                log("校验异常（忽略）:", e)

        out["ok"] = True
        out["took_ms"] = int((time.time() - t0) * 1000)
        log("入池结果:", json.dumps({k: out.get(k) for k in
                                 ("id", "cookie_len", "proxy_id", "refreshes_count",
                                  "check", "check_skipped")},
                                ensure_ascii=False)[:400])
    finally:
        # 默认保留浏览器常驻（见上方 v1.1.0 说明）。--keep-open/--no-stop 是
        # 兼容别名，三者等价；只有 --stop 才真的关掉。
        if stop_after and not keep_open and not no_stop:
            stop_browser(profile)

    print(json.dumps(out, ensure_ascii=False))
    return 0 if out["ok"] else 4


if __name__ == "__main__":
    sys.exit(main())
