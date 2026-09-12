#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""gw2a-cookie-watchdog —— 保活/入池的兜底调度与健康巡检。

扩展自身用 chrome.alarms 每 10 分钟保活、每 30 分钟入池；但 MV3 的
service worker 可能被回收，且我们需要一份可观测的记录。本脚本由
systemd timer 每 5 分钟跑一次：

  * 若扩展的 lastKeepaliveAt 已过期（> GW2A_KEEPALIVE_MAX_SEC），补一次保活
  * 若扩展的 lastSyncAt 已过期（> GW2A_SYNC_MAX_SEC），补一次入池
  * 若未登录，只记录告警（不反复折腾页面）
  * 结果写入状态文件，便于外部监控

环境变量：
  GW2A_KEEPALIVE_MAX_SEC  默认 900  (15min)
  GW2A_SYNC_MAX_SEC       默认 2100 (35min)
  GW2A_STATUS_FILE        默认 /var/log/gw2a-cookie-sync-status.json
  GW2A_PROFILE            默认 acct1
"""
import json
import os
import subprocess
import sys
import time

GW2A_SYNC = "/opt/gw2a-cookie-sync/gw2a-sync.py"
PY = "/opt/gw2a-cookie-sync/venv/bin/python"
KEEPALIVE_MAX = int(os.environ.get("GW2A_KEEPALIVE_MAX_SEC", "900"))
SYNC_MAX = int(os.environ.get("GW2A_SYNC_MAX_SEC", "2100"))
STATUS_FILE = os.environ.get("GW2A_STATUS_FILE", "/var/log/gw2a-cookie-sync-status.json")
PROFILE = os.environ.get("GW2A_PROFILE", "acct1")


def run_sync(*args, timeout=180):
    cmd = [PY, GW2A_SYNC] + list(args)
    try:
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        return p.returncode, p.stdout.decode("utf-8", "replace"), p.stderr.decode("utf-8", "replace")
    except subprocess.TimeoutExpired:
        return 124, "", "timeout"
    except Exception as e:
        return 1, "", "%s: %s" % (type(e).__name__, e)


def main():
    now = int(time.time())
    status = {
        "checked_at": now,
        "profile": PROFILE,
        "actions": [],
        "logged_in": None,
        "last_keepalive_age_sec": None,
        "last_sync_age_sec": None,
        "last_sync_ok": None,
        "last_sync_detail": None,
        "pool_count": None,
        "ok": True,
        "errors": [],
    }

    rc, out, err = run_sync("state", PROFILE, timeout=180)
    st = None
    try:
        st = json.loads(out.strip().splitlines()[-1]) if out.strip() else None
    except Exception:
        st = None

    if not st:
        status["ok"] = False
        status["errors"].append("state 解析失败 rc=%d err=%s out=%s"
                                % (rc, err[:200], out[:300]))
    else:
        if st.get("error"):
            status["ok"] = False
            status["errors"].append(st["error"])
        ext = (st.get("extension") or {}).get("resp") or {}
        cfg = ext.get("config") or {}
        status["logged_in"] = ext.get("logged_in")
        last_ka = cfg.get("lastKeepaliveAt") or 0
        last_sy = cfg.get("lastSyncAt") or 0
        status["last_keepalive_age_sec"] = round(now - last_ka / 1000.0) if last_ka else None
        status["last_sync_age_sec"] = round(now - last_sy / 1000.0) if last_sy else None
        status["last_sync_ok"] = cfg.get("lastSyncOk")
        status["last_sync_detail"] = cfg.get("lastSyncDetail")
        status["last_keepalive_detail"] = cfg.get("lastKeepaliveDetail")
        pool = st.get("pool") or {}
        status["pool_count"] = len(pool.get("items") or [])

    # 保活兜底
    ka_age = status["last_keepalive_age_sec"]
    if status["logged_in"] is False:
        status["actions"].append({"keepalive": "skipped (not logged in)"})
    elif ka_age is None or ka_age > KEEPALIVE_MAX:
        rc3, out3, err3 = run_sync("keepalive", PROFILE, timeout=180)
        status["actions"].append({"keepalive": {"rc": rc3, "err": err3[-200:]}})

    # 入池兜底
    sy_age = status["last_sync_age_sec"]
    if not status["logged_in"]:
        status["actions"].append({"sync": "skipped (not logged in)"})
    elif sy_age is None or sy_age > SYNC_MAX:
        rc4, out4, err4 = run_sync("sync", PROFILE, timeout=180)
        status["actions"].append({"sync": {"rc": rc4, "err": err4[-200:]}})

    if status["logged_in"] is False:
        status["ok"] = False
        status["errors"].append(
            "Gemini 未登录：请在 VNC 桌面 http://NAS_HOST:16100/ 完成 Google 登录")

    try:
        with open(STATUS_FILE, "w", encoding="utf-8") as f:
            json.dump(status, f, ensure_ascii=False, indent=2)
            f.write("\n")
    except Exception as e:
        print("status write failed:", e)

    print(json.dumps(status, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
