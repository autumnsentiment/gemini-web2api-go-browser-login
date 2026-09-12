#!/usr/bin/env python
"""End-to-end test: inject fake Google auth cookies via CDP, ask the extension
to sync, and confirm the cookie reaches the controller + pool."""
import json
import sqlite3
import sys
import time

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json  # noqa: E402
import urllib.request  # noqa: E402

PORT = 9300
CTRL = "http://127.0.0.1:9280"
EID = "ommhbjlpchdoohbeamepaddkoidokelc"
MARK = "E2ETESTVALUE12345"
DB = "/vol2/docker/volumes/gemini-web2api-go_gw2a-data/_data/gemini.db"

EXPR = """
new Promise((resolve) => {
  try {
    chrome.runtime.sendMessage(%s, {type:'sync'}, (resp) => {
      resolve(JSON.stringify({resp: resp,
        err: chrome.runtime.lastError ? chrome.runtime.lastError.message : null}));
    });
  } catch (e) { resolve(JSON.stringify({err: String(e)})); }
})
""" % json.dumps(EID)

LIST_EXPR = """
new Promise((resolve) => {
  chrome.cookies.getAll({domain:'.google.com'}, (list) => {
    resolve(JSON.stringify(list.map(c => c.name + '=' + c.value.slice(0,20))));
  });
})
"""


def ctrl(path, method="GET", body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(CTRL + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read().decode())


def main():
    bws = http_json("http://127.0.0.1:%d/json/version" % PORT)["webSocketDebuggerUrl"]
    b = CDP(bws)
    tid = None
    try:
        tid = b.send("Target.createTarget", {"url": "https://gemini.google.com/app",
                                             "background": True})["targetId"]
        sid = b.send("Target.attachToTarget", {"targetId": tid, "flatten": True})["sessionId"]
        b.send("Network.enable", session_id=sid)
        for name in ("SAPISID", "SID", "__Secure-1PAPISID", "__Secure-1PSID"):
            r = b.send("Network.setCookie", {
                "name": name, "value": MARK, "domain": ".google.com",
                "path": "/", "secure": True, "httpOnly": True,
                "expires": time.time() + 3600,
            }, session_id=sid)
            print("setCookie %-20s -> %s" % (name, r))
        # 确认浏览器确实写入了
        got = b.send("Network.getCookies", {"urls": ["https://gemini.google.com/app"]},
                     session_id=sid)
        names = sorted(c["name"] for c in got.get("cookies", []))
        print("cookies visible to page:", names)
        print("MARKER present:", any(c["value"] == MARK for c in got.get("cookies", [])))

        bridge = b.send("Target.createTarget", {"url": CTRL + "/extension",
                                                "background": True})["targetId"]
        bsid = b.send("Target.attachToTarget", {"targetId": bridge, "flatten": True})["sessionId"]
        b.send("Runtime.enable", session_id=bsid)
        for _ in range(120):
            r = b.send("Runtime.evaluate",
                       {"expression": "location.href + '|' + document.readyState",
                        "returnByValue": True}, session_id=bsid)
            v = r.get("result", {}).get("value") or ""
            if v.startswith(CTRL + "/extension") and v.endswith("complete"):
                break
            time.sleep(0.25)
        print("bridge ready:", v)
        # 桥接页是否属于扩展？（能调用 chrome.cookies 才行）
        r = b.send("Runtime.evaluate", {"expression": "location.origin",
                                        "returnByValue": True}, session_id=bsid)
        print("bridge origin:", r.get("result", {}).get("value"))
        r = b.send("Runtime.evaluate", {"expression": LIST_EXPR, "awaitPromise": True,
                                        "returnByValue": True}, session_id=bsid, timeout=40)
        print("bridge sees cookies:", r.get("result", {}).get("value"))

        r = b.send("Runtime.evaluate", {"expression": EXPR, "awaitPromise": True,
                                        "returnByValue": True}, session_id=bsid, timeout=120)
        print("extension sync ->", r.get("result", {}).get("value"))

        print("pool:", json.dumps(ctrl("/cookie-pool"), ensure_ascii=False)[:700])
        con = sqlite3.connect("file:%s?mode=ro" % DB, uri=True)
        rows = list(con.execute(
            "select id,label,profile,status,length(cookie),substr(cookie,1,80) "
            "from accounts where source='browser'"))
        print("db browser rows:")
        for row in rows:
            print("  ", row, "| MARKER:", MARK in (row[5] or ""))

        for name in ("SAPISID", "SID", "__Secure-1PAPISID", "__Secure-1PSID"):
            b.send("Network.deleteCookies", {"name": name, "domain": ".google.com"},
                   session_id=sid)
        print("cleaned fake cookies")
        b.send("Target.closeTarget", {"targetId": bridge})
    finally:
        if tid:
            try:
                b.send("Target.closeTarget", {"targetId": tid})
            except Exception:
                pass
        b.close()


if __name__ == "__main__":
    main()
