#!/usr/bin/env python
"""Probe the Chromium profile's real Google/Gemini login state via CDP."""
import json
import sys
import time

sys.path.insert(0, "/opt/gw2a-cookie-sync")
from cdp import CDP, http_json  # noqa: E402

CDP_HTTP = "http://127.0.0.1:9300"


def main():
    ver = http_json(CDP_HTTP + "/json/version")
    bws = ver["webSocketDebuggerUrl"]
    print("browser ws:", bws)
    b = CDP(bws)

    # browser-level cookie store
    res = b.send("Storage.getCookies")
    cookies = res.get("cookies", [])
    print("total cookies:", len(cookies))
    g = [c for c in cookies if "google" in c["domain"]]
    print("google-domain cookies:", len(g))
    names = sorted({c["name"] for c in g})
    print("names:", names)
    key = ["SID", "HSID", "SSID", "APISID", "SAPISID", "__Secure-1PSID", "__Secure-3PSID"]
    have = {k: any(c["name"] == k for c in g) for k in key}
    print("key presence:", have)
    # show expiry of the critical ones
    now = time.time()
    for c in sorted(g, key=lambda x: x["name"]):
        if c["name"] in key or c["name"].startswith("__Secure-"):
            exp = c.get("expires", -1)
            left = (exp - now) / 3600.0 if exp and exp > 0 else None
            print("  %-22s domain=%-22s secure=%s httpOnly=%s session=%s left_h=%.1f"
                  % (c["name"], c["domain"], c.get("secure"), c.get("httpOnly"),
                     c.get("session"), left if left is not None else -1))

    # attach to gemini page and evaluate
    targets = b.send("Target.getTargets")["targetInfos"]
    page = None
    for t in targets:
        if t["type"] == "page" and "gemini.google.com" in (t.get("url") or ""):
            page = t
            break
    if not page:
        for t in targets:
            if t["type"] == "page":
                page = t
                break
    print("page target:", page and (page["url"], page["targetId"]))
    if not page:
        print("no page target")
        b.close()
        return

    att = b.send("Target.attachToTarget", {"targetId": page["targetId"], "flatten": True})
    sid = att["sessionId"]

    b.send("Runtime.enable", session_id=sid)
    # check SNlM0e / login markers
    expr = """(() => {
      const t = document.documentElement.innerHTML || '';
      const m = t.match(/SNlM0e\\s*[:=]\\s*"(.*?)"/);
      const m2 = t.match(/SNlM0e\\"?:?\\s*\\"?([A-Za-z0-9_\\-:]{20,})/);
      return JSON.stringify({
        url: location.href,
        title: document.title,
        snlm0e_found: !!m,
        snlm0e_tail: m ? m[1].slice(-12) : null,
        has_signin: /Sign in|登录/.test(document.body ? document.body.innerText.slice(0,3000) : ''),
        body_head: (document.body ? document.body.innerText.slice(0, 300) : ''),
        wiz_global: t.indexOf('WIZ_global_data') >= 0,
      });
    })()"""
    r = b.send("Runtime.evaluate", {"expression": expr, "returnByValue": True,
                                    "awaitPromise": False}, session_id=sid)
    print("EVAL:", json.dumps(r.get("result", {}).get("value"), ensure_ascii=False))
    b.close()


if __name__ == "__main__":
    main()
