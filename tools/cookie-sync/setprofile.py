#!/usr/bin/env python
"""Set the extension's profile label (and confirm), then clean test data."""
import json
import sqlite3
import sys
import time

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json  # noqa: E402

PORT = 9300
CTRL = "http://127.0.0.1:9280"
EID = "ommhbjlpchdoohbeamepaddkoidokelc"
DB = "/vol2/docker/volumes/gemini-web2api-go_gw2a-data/_data/gemini.db"


def main():
    bws = http_json("http://127.0.0.1:%d/json/version" % PORT)["webSocketDebuggerUrl"]
    b = CDP(bws)
    tid = None
    try:
        tid = b.send("Target.createTarget", {"url": CTRL + "/extension",
                                             "background": True})["targetId"]
        sid = b.send("Target.attachToTarget", {"targetId": tid, "flatten": True})["sessionId"]
        b.send("Runtime.enable", session_id=sid)
        for _ in range(120):
            r = b.send("Runtime.evaluate",
                       {"expression": "location.href + '|' + document.readyState",
                        "returnByValue": True}, session_id=sid)
            v = r.get("result", {}).get("value") or ""
            if v.startswith(CTRL + "/extension") and v.endswith("complete"):
                break
            time.sleep(0.25)

        expr = """
        new Promise((resolve) => {
          chrome.runtime.sendMessage(%s, {type:'status', patch:{profile:'acct1'}},
            (resp) => resolve(JSON.stringify({
              resp: resp,
              err: chrome.runtime.lastError ? chrome.runtime.lastError.message : null})));
        })""" % json.dumps(EID)
        r = b.send("Runtime.evaluate", {"expression": expr, "awaitPromise": True,
                                        "returnByValue": True}, session_id=sid, timeout=60)
        print("set profile ->", r.get("result", {}).get("value"))
    finally:
        if tid:
            try:
                b.send("Target.closeTarget", {"targetId": tid})
            except Exception:
                pass
        b.close()

    # 清理测试账号
    con = sqlite3.connect(DB, timeout=15)
    con.execute("delete from accounts where source='browser'")
    con.commit()
    print("pool after cleanup:", list(con.execute(
        "select id,label,source,status from accounts")))
    con.close()


if __name__ == "__main__":
    main()
