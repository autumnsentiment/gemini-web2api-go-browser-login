#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Patch gw2a-browser-controller.service with cookie-sync env vars (idempotent)."""
import io
import os
import re
import sys

UNIT = "/etc/systemd/system/gw2a-browser-controller.service"
EXT_ID = "ommhbjlpchdoohbeamepaddkoidokelc"
ADMIN_TOKEN = os.environ.get("GW2A_ADMIN_TOKEN", "")

WANT = [
    ("GW2A_API", "http://127.0.0.1:8083"),
    ("GW2A_ADMIN_TOKEN", ADMIN_TOKEN),
    ("GW2A_EXT_ID", EXT_ID),
    ("GW2A_SYNC_TOKEN", ""),
]

with io.open(UNIT, "r", encoding="utf-8") as f:
    lines = f.read().split("\n")

out = []
existing = {}
for ln in lines:
    m = re.match(r"^Environment=([A-Za-z0-9_]+)=(.*)$", ln)
    if m:
        existing[m.group(1)] = ln
    out.append(ln)

# insert any missing vars right after the last Environment= line in [Service]
to_add = [(k, v) for k, v in WANT if k not in existing]
if to_add:
    idx = max(i for i, ln in enumerate(out) if ln.startswith("Environment="))
    for k, v in reversed(to_add):
        out.insert(idx + 1, "Environment=%s=%s" % (k, v))

new = "\n".join(out)
if new != "\n".join(lines):
    with io.open(UNIT, "w", encoding="utf-8", newline="\n") as f:
        f.write(new)
    print("unit updated; added:", [k for k, _ in to_add] or "none")
else:
    print("unit already up to date")

print("--- resulting unit ---")
print(new)
sys.exit(0)
