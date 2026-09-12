#!/usr/bin/env python
"""Dump Google auth-cookie presence for a Chromium profile's Cookies DB (read-only copy)."""
import os
import shutil
import sqlite3
import sys
import tempfile
import time

KEY = ["SID", "HSID", "SSID", "APISID", "SAPISID", "ACCOUNT_CHOOSER",
       "__Secure-1PSID", "__Secure-1PSIDTS", "__Secure-3PSID", "SIDCC"]


def inspect(profile_dir):
    src = os.path.join(profile_dir, "Default", "Cookies")
    if not os.path.exists(src):
        src = os.path.join(profile_dir, "Cookies")
    if not os.path.exists(src):
        print("  [%s] no Cookies db" % profile_dir)
        return
    tmp = tempfile.mkdtemp()
    for suf in ("", "-wal", "-shm"):
        p = src + suf
        if os.path.exists(p):
            shutil.copy2(p, os.path.join(tmp, "Cookies" + suf))
    db = os.path.join(tmp, "Cookies")
    try:
        con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
        rows = list(con.execute(
            "select host_key,name,length(value),length(encrypted_value),expires_utc,"
            "is_secure,is_httponly from cookies"))
    except Exception as e:
        print("  [%s] ERR %s" % (profile_dir, e))
        return
    finally:
        pass
    g = [r for r in rows if "google" in r[0]]
    names = sorted({r[1] for r in g})
    have = [k for k in KEY if k in names]
    now_us = time.time() * 1e6
    alive = [r[1] for r in g if r[1] in KEY and r[4] and r[4] > now_us]
    print("  [%s]" % profile_dir)
    print("     total=%d google=%d" % (len(rows), len(g)))
    print("     auth names present: %s" % (have or "NONE"))
    print("     auth unexpired: %s" % (alive or "NONE"))
    print("     all google names: %s" % names)
    shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    for d in sys.argv[1:]:
        inspect(d)
