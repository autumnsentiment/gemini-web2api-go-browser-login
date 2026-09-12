#!/usr/bin/env python
"""Inspect the current Gemini page state + navigate to login if needed."""
import json
import sys
import time

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json  # noqa: E402

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9301


def page_ws():
    for t in http_json("http://127.0.0.1:%d/json/list" % PORT):
        if t.get("type") == "page" and "chrome-extension" not in (t.get("url") or ""):
            return t
    return None


def main():
    t = page_ws()
    if not t:
        print("no page")
        return
    print("target:", t.get("url"))
    b = CDP(t["webSocketDebuggerUrl"])
    try:
        b.send("Page.enable")
        b.send("Runtime.enable")
        expr = """JSON.stringify({
          url: location.href,
          title: document.title,
          head: (document.body ? document.body.innerText : '').slice(0, 600),
        })"""
        r = b.send("Runtime.evaluate", {"expression": expr, "returnByValue": True})
        print(json.dumps(json.loads(r["result"]["value"]), ensure_ascii=False, indent=1))
    finally:
        b.close()


if __name__ == "__main__":
    main()
