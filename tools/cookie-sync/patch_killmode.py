#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Ensure KillMode=process so controller restarts don't kill adopted Chromium windows."""
import io
import re

UNIT = "/etc/systemd/system/gw2a-browser-controller.service"
with io.open(UNIT, "r", encoding="utf-8") as f:
    s = f.read()

if re.search(r"^KillMode=", s, re.M):
    s = re.sub(r"^KillMode=.*$", "KillMode=process", s, flags=re.M)
    print("KillMode replaced")
else:
    # insert after RestartSec= line
    m = re.search(r"^RestartSec=.*$", s, re.M)
    assert m, "RestartSec not found"
    s = s[:m.end()] + "\nKillMode=process" + s[m.end():]
    print("KillMode added")

with io.open(UNIT, "w", encoding="utf-8", newline="\n") as f:
    f.write(s)
print("--- unit ---")
print(s)
