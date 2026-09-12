#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Set GW2A_BROWSER_PROXY in the controller unit (idempotent)."""
import io
import re
import sys

UNIT = "/etc/systemd/system/gw2a-browser-controller.service"
PROXY = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:7890"

with io.open(UNIT, "r", encoding="utf-8") as f:
    s = f.read()

line = "Environment=GW2A_BROWSER_PROXY=%s" % PROXY
if re.search(r"^Environment=GW2A_BROWSER_PROXY=.*$", s, re.M):
    s = re.sub(r"^Environment=GW2A_BROWSER_PROXY=.*$", line, s, flags=re.M)
    print("replaced")
else:
    m = re.search(r"^Environment=GW2A_SYNC_TOKEN=.*$", s, re.M)
    assert m, "GW2A_SYNC_TOKEN anchor not found"
    s = s[:m.end()] + "\n" + line + s[m.end():]
    print("added")

with io.open(UNIT, "w", encoding="utf-8", newline="\n") as f:
    f.write(s)
print(s)
