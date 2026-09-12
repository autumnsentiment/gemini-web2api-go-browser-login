#!/usr/bin/env python
"""Check whether the browser can reach loopback (needed by the extension)."""
import json
import sys
import time

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json  # noqa: E402

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9300


def main():
    bws = http_json("http://127.0.0.1:%d/json/version" % PORT)["webSocketDebuggerUrl"]
    b = CDP(bws)
    tid = None
    try:
        for url in ("http://127.0.0.1:9280/extension-id",
                    "http://localhost:9280/extension-id"):
            tid = b.send("Target.createTarget", {"url": url, "background": True})["targetId"]
            sid = b.send("Target.attachToTarget", {"targetId": tid, "flatten": True})["sessionId"]
            b.send("Runtime.enable", session_id=sid)
            for _ in range(40):
                r = b.send("Runtime.evaluate", {"expression": "document.readyState",
                                                "returnByValue": True}, session_id=sid)
                if r.get("result", {}).get("value") == "complete":
                    break
                time.sleep(0.25)
            r = b.send("Runtime.evaluate", {
                "expression": "JSON.stringify({url:location.href,origin:location.origin,"
                              "text:(document.body?document.body.innerText:'').slice(0,200)})",
                "returnByValue": True}, session_id=sid)
            print(url, "->", r.get("result", {}).get("value"))
            b.send("Target.closeTarget", {"targetId": tid})
            tid = None
    finally:
        if tid:
            try:
                b.send("Target.closeTarget", {"targetId": tid})
            except Exception:
                pass
        b.close()


if __name__ == "__main__":
    main()
