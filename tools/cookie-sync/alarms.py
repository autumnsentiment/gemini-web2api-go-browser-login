#!/usr/bin/env python
"""Read the extension service worker's chrome.alarms + storage (confirms scheduling)."""
import json
import sys
import time

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json  # noqa: E402

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9300
EID = "ommhbjlpchdoohbeamepaddkoidokelc"

EXPR = """
new Promise((resolve) => {
  try {
    chrome.alarms.getAll((list) => {
      chrome.storage.local.get(null, (all) => {
        resolve(JSON.stringify({ alarms: list, cfg: all }));
      });
    });
  } catch (e) { resolve(JSON.stringify({ error: String(e) })); }
})
"""


def main():
    bws = http_json("http://127.0.0.1:%d/json/version" % PORT)["webSocketDebuggerUrl"]
    b = CDP(bws)
    tid = None
    try:
        # 让扩展的 service worker 唤醒
        tid = b.send("Target.createTarget", {"url": "https://gemini.google.com/app",
                                             "background": True})["targetId"]
        sw = None
        for _ in range(30):
            for t in b.send("Target.getTargets")["targetInfos"]:
                if t["type"] == "service_worker" and EID in (t.get("url") or ""):
                    sw = t
                    break
            if sw:
                break
            time.sleep(0.4)
        if not sw:
            print("no service worker target found")
            return 1
        sid = b.send("Target.attachToTarget",
                     {"targetId": sw["targetId"], "flatten": True})["sessionId"]
        b.send("Runtime.enable", session_id=sid)
        r = b.send("Runtime.evaluate",
                   {"expression": EXPR, "awaitPromise": True, "returnByValue": True},
                   session_id=sid, timeout=40)
        val = r.get("result", {}).get("value")
        d = json.loads(val) if val else {"raw": r}
        print("config:")
        for k in sorted(d.get("cfg") or {}):
            v = (d["cfg"])[k]
            if k.endswith("At") and isinstance(v, (int, float)) and v:
                v = "%s (%s)" % (v, time.strftime("%H:%M:%S", time.localtime(v / 1000)))
            print("  %-22s %s" % (k, v))
        print("alarms:")
        for a in d.get("alarms") or []:
            print("  %-16s period=%s min  next=%s" % (
                a.get("name"), a.get("periodInMinutes"),
                time.strftime("%H:%M:%S", time.localtime(a.get("scheduledTime", 0) / 1000))))
        print("ALARM_NAMES=%s" % sorted(a.get("name") for a in (d.get("alarms") or [])))
    finally:
        if tid:
            try:
                b.send("Target.closeTarget", {"targetId": tid})
            except Exception:
                pass
        b.close()


if __name__ == "__main__":
    main()
