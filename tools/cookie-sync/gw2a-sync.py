#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""gw2a-sync —— 命令行入口：查看状态 / 触发保活 / 触发入池 / 打开登录页。

用法:
  gw2a-sync state [profile]         # 单一 JSON 输出（供脚本/监控解析）
  gw2a-sync status [profile]        # 人类可读状态
  gw2a-sync keepalive [profile]     # 让扩展刷新 Gemini 页面保活
  gw2a-sync sync [profile]          # 让扩展立刻抓 cookie 入池
  gw2a-sync pool                    # 只看 cookie 池里 browser 来源的账号
  gw2a-sync open [profile]          # 打开 gemini 登录页（配合 VNC 登录）

原理：扩展声明了 externally_connectable，允许 127.0.0.1 页面用
chrome.runtime.sendMessage(EXT_ID, ...) 驱动它。本脚本通过 CDP 在
控制器托管的 /extension 桥接页里发起同样的调用（用独立临时标签页，
避免干扰正在被保活刷新的 gemini 标签页）。
"""
import json
import os
import sys
import time
import urllib.request

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json  # noqa: E402

CTRL = os.environ.get("GW2A_CTRL_URL", "http://127.0.0.1:9280").rstrip("/")
CDP_BASE = int(os.environ.get("GW2A_CDP_BASE", "9300"))
BRIDGE = CTRL + "/extension"


def ctrl_json(path, method="GET", body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(CTRL + path, data=data, method=method,
                                headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=40) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


def ext_id():
    return ctrl_json("/extension-id")["ext_id"]


def browser_ws(port):
    return http_json("http://127.0.0.1:%d/json/version" % port)["webSocketDebuggerUrl"]


def call_ext(port, msg, timeout=180):
    """在临时桥接标签页里执行 chrome.runtime.sendMessage(EXT_ID, msg)。"""
    b = CDP(browser_ws(port))
    tid = None
    try:
        tid = b.send("Target.createTarget", {"url": BRIDGE, "background": True})["targetId"]
        sid = b.send("Target.attachToTarget", {"targetId": tid, "flatten": True})["sessionId"]
        b.send("Runtime.enable", session_id=sid)
        # 等桥接页真正导航完成（刚 createTarget 时它可能还停在 about:blank，
        # 那时 origin 是 null、chrome.runtime 不可用）
        for _ in range(120):
            r = b.send("Runtime.evaluate",
                       {"expression": "location.href + '|' + document.readyState",
                        "returnByValue": True}, session_id=sid)
            v = r.get("result", {}).get("value") or ""
            if v.startswith(BRIDGE) and v.endswith("complete"):
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("bridge page did not load: %s" % v)
        eid = ext_id()
        expr = """
        new Promise((resolve) => {
          try {
            chrome.runtime.sendMessage(%s, %s, (resp) => {
              resolve(JSON.stringify({ ok: true, resp: resp,
                err: chrome.runtime.lastError ? chrome.runtime.lastError.message : null }));
            });
          } catch (e) { resolve(JSON.stringify({ ok: false, err: String(e) })); }
        })
        """ % (json.dumps(eid), json.dumps(msg))
        r = b.send("Runtime.evaluate",
                   {"expression": expr, "awaitPromise": True, "returnByValue": True},
                   session_id=sid, timeout=timeout)
        val = r.get("result", {}).get("value")
        return json.loads(val) if val else {"error": "no result", "raw": r}
    finally:
        if tid:
            try:
                b.send("Target.closeTarget", {"targetId": tid})
            except Exception:
                pass
        b.close()


def ensure_profile(profile):
    profs = ctrl_json("/profiles").get("profiles", [])
    for p in profs:
        if p["name"] == profile:
            return p["port"]
    r = ctrl_json("/profiles", "POST", {"name": profile})
    print("launched profile %s -> %s" % (profile, json.dumps(r, ensure_ascii=False)),
          file=sys.stderr)
    time.sleep(8)
    return r.get("port", CDP_BASE)


def state(profile):
    """单一 JSON 输出，便于脚本/监控解析。"""
    out = {"ts": int(time.time()), "controller": None, "profiles": None,
           "pool": None, "extension": None, "error": None}
    try:
        out["controller"] = ctrl_json("/healthz")
        out["profiles"] = ctrl_json("/profiles")
        out["pool"] = ctrl_json("/cookie-pool")
    except Exception as e:
        out["error"] = "controller unreachable: %s" % e
        print(json.dumps(out, ensure_ascii=False))
        return 1
    try:
        port = ensure_profile(profile)
        out["extension"] = call_ext(port, {"type": "status"}, timeout=60)
    except Exception as e:
        out["error"] = "extension unreachable: %s" % e
    print(json.dumps(out, ensure_ascii=False))
    return 0


def main():
    args = sys.argv[1:]
    cmd = args[0] if args else "status"
    profile = args[1] if len(args) > 1 else "acct1"

    if cmd == "pool":
        print(json.dumps(ctrl_json("/cookie-pool"), ensure_ascii=False, indent=2))
        return 0

    if cmd == "state":
        return state(profile)

    if cmd == "open":
        port = ensure_profile(profile)
        b = CDP(browser_ws(port))
        try:
            b.send("Target.createTarget", {"url": "https://gemini.google.com/app"})
        finally:
            b.close()
        print("已在 Chromium 打开 gemini.google.com。")
        print("请通过 VNC 桌面完成 Google 登录：http://NAS_HOST:16100/")
        print("登录后扩展会自动把 cookie 入池（也可执行 gw2a-sync sync）。")
        return 0

    port = ensure_profile(profile)

    if cmd == "status":
        st = state(profile)
        print("controller:", json.dumps(ctrl_json("/healthz"), ensure_ascii=False))
        print("profiles  :", json.dumps(ctrl_json("/profiles"), ensure_ascii=False))
        print("pool      :", json.dumps(ctrl_json("/cookie-pool"), ensure_ascii=False))
        return st

    if cmd not in ("keepalive", "sync"):
        print(__doc__)
        return 2

    out = call_ext(port, {"type": cmd})
    print(json.dumps(out, ensure_ascii=False, indent=2))
    resp = (out or {}).get("resp") or {}
    return 0 if resp.get("ok") else 1


if __name__ == "__main__":
    sys.exit(main())
