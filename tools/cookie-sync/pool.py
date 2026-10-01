#!/usr/bin/env python3
"""gemini-web2api cookie 池读写助手（宿主机侧，直接操作容器共享的 SQLite）。

子命令：
  upsert   从 stdin 读 JSON {profile,label,cookie,note,source,authuser}，按 (source,profile,authuser)
           找到既有账号则更新 cookie（保号），否则新增；同时删除该 profile 的
           其它同槽位历史记录（只留最新一条）。输出 JSON {id, action, removed}
  set_ok   从 stdin 读 JSON {id}：标记账号可用（status=enabled, last_ok_at=now, fail_count=0）
  set_fail 从 stdin 读 JSON {id,error}：标记账号失败（last_error, fail_count+1）
  set_proxy 从 stdin 读 JSON {id,proxy_id}：把账号绑定到指定出口（0=直连）
  ext_status 从 stdin 读 JSON {profile,mode,logged_in,cookie_count,detail}：记录扩展心跳/模式到 kv
  list     列出浏览器来源的账号（不含完整 cookie）
  get      从 stdin 读 JSON {id}：输出该账号（含 cookie）

设计要点：
  * 只用标准库 sqlite3，无需在宿主机装 sqlite3 CLI。
  * WAL + busy_timeout，与容器内进程并发写安全。
  * 不修改文件属主，容器内 nonroot 仍可读写。
"""
import json
import os
import sqlite3
import sys
import time

DB_DEFAULT = "/vol2/docker/volumes/gemini-web2api-go_gw2a-data/_data/gemini.db"
DB = os.environ.get("GW2A_DB_PATH", DB_DEFAULT)


def connect():
    con = sqlite3.connect(DB, timeout=15, isolation_level=None)
    con.execute("PRAGMA busy_timeout=15000")
    con.execute("PRAGMA journal_mode=WAL")
    return con


def now():
    return int(time.time())


def read_stdin_json():
    raw = sys.stdin.read() or "{}"
    return json.loads(raw)


def cmd_upsert():
    d = read_stdin_json()
    profile = (d.get("profile") or "").strip()
    cookie = (d.get("cookie") or "").strip()
    label = (d.get("label") or "").strip()
    note = (d.get("note") or "").strip()
    source = (d.get("source") or "browser").strip()
    authuser = int(d.get("authuser") or 0)
    # Defense in depth: old extensions represented Google URL slots as
    # separate profiles (browser1-u1 ... -u9), creating phantom accounts.
    import re
    if re.search(r"-u\d+$", profile):
        print(json.dumps({"error": "reject multi-account suffix profile"}))
        return 2
    if not cookie:
        print(json.dumps({"error": "empty cookie"}))
        return 2
    con = connect()
    try:
        # 2026-09-24: dedupe by (profile, authuser).
        # Google multi-account shares the same cookie; switching accounts is via
        # /u/N/ URL. Extension writes different profiles per slot (browser1-u1).
        # Adding authuser prevents different slots on the same base profile
        # (fallback case) from overwriting each other.
        row = con.execute(
            "SELECT id FROM accounts WHERE profile=? AND authuser=? "
            "ORDER BY id DESC LIMIT 1",
            (profile, authuser)).fetchone()
        t = now()
        if row:
            aid = row[0]
            con.execute(
                "UPDATE accounts SET cookie=?, label=?, note=?, status='enabled', "
                "last_error='', last_ok_at=?, fail_count=0, authuser=? WHERE id=?",
                (cookie, label, note, t, authuser, aid))
            action = "updated"
        else:
            cur = con.execute(
                "INSERT INTO accounts (label, cookie, status, note, created_at, "
                "last_used_at, last_ok_at, last_error, fail_count, proxy_id, source, profile, authuser) "
                "VALUES (?,?,'enabled',?,?,0,?,'',0,0,?,?,?)",
                (label, cookie, note, t, t, source, profile, authuser))
            aid = cur.lastrowid
            action = "inserted"
        # 清掉同一 profile 同槽位的其它历史记录。不同槽位是不同账号，不能误删。
        removed = con.execute(
            "DELETE FROM accounts WHERE profile=? AND authuser=? AND id<>?",
            (profile, authuser, aid)).rowcount
        print(json.dumps({"id": aid, "action": action, "profile": profile,
                          "authuser": authuser, "cookie_len": len(cookie),
                          "ts": t, "removed": removed}))
        return 0
    finally:
        con.close()


def cmd_set_ok():
    d = read_stdin_json()
    con = connect()
    try:
        con.execute("UPDATE accounts SET status='enabled', last_error='', "
                    "last_ok_at=?, fail_count=0 WHERE id=?", (now(), int(d["id"])))
        print(json.dumps({"ok": True}))
    finally:
        con.close()


def cmd_set_fail():
    d = read_stdin_json()
    con = connect()
    try:
        con.execute("UPDATE accounts SET last_error=?, fail_count=fail_count+1 "
                    "WHERE id=?", ((d.get("error") or "")[:500], int(d["id"])))
        print(json.dumps({"ok": True}))
    finally:
        con.close()


def cmd_set_proxy():
    d = read_stdin_json()
    con = connect()
    try:
        con.execute("UPDATE accounts SET proxy_id=? WHERE id=?",
                    (int(d.get("proxy_id") or 0), int(d["id"])))
        print(json.dumps({"ok": True, "id": int(d["id"]),
                          "proxy_id": int(d.get("proxy_id") or 0)}))
    finally:
        con.close()

def cmd_ext_status():
    d = read_stdin_json()
    profile = (d.get("profile") or "").strip()
    mode = str(d.get("mode") or "controller").strip().lower()
    if mode not in ("controller", "service"):
        mode = "controller"
    if not profile:
        print(json.dumps({"error": "empty profile"}))
        return 2
    import re
    if re.search(r"-u\d+$", profile):
        # Legacy multi-account extensions reported one fake profile per slot.
        print(json.dumps({"error": "reject multi-account suffix profile"}))
        return 2
    t = now()
    con = connect()
    try:
        rows = {
            "ext_mode:" + profile: mode,
            "ext_seen_at:" + profile: str(t),
            "ext_logged_in:" + profile: "1" if d.get("logged_in") else "0",
            "ext_cookie_count:" + profile: str(int(d.get("cookie_count") or 0)),
        }
        detail = str(d.get("detail") or "")[:300]
        if detail:
            rows["ext_last_detail:" + profile] = detail
        for k, v in rows.items():
            con.execute("INSERT INTO kv (k, v) VALUES (?, ?) "
                        "ON CONFLICT(k) DO UPDATE SET v=excluded.v", (k, v))
        print(json.dumps({"ok": True, "profile": profile, "mode": mode, "ts": t}))
        return 0
    finally:
        con.close()

def cmd_list():
    con = connect()
    try:
        rows = con.execute(
            "SELECT id,label,status,source,profile,length(cookie),last_used_at,"
            "last_ok_at,last_error,fail_count FROM accounts "
            "WHERE source='browser' ORDER BY id").fetchall()
        cols = ["id", "label", "status", "source", "profile", "cookie_len",
                "last_used_at", "last_ok_at", "last_error", "fail_count"]
        print(json.dumps({"items": [dict(zip(cols, r)) for r in rows]}))
    finally:
        con.close()


def cmd_get():
    d = read_stdin_json()
    con = connect()
    try:
        row = con.execute(
            "SELECT id,label,cookie,status,profile,last_error,fail_count,last_ok_at "
            "FROM accounts WHERE id=?", (int(d["id"]),)).fetchone()
        if not row:
            print(json.dumps({"error": "not found"}))
            return 1
        cols = ["id", "label", "cookie", "status", "profile", "last_error",
                "fail_count", "last_ok_at"]
        print(json.dumps(dict(zip(cols, row))))
    finally:
        con.close()


CMDS = {
    "upsert": cmd_upsert, "set_ok": cmd_set_ok, "set_fail": cmd_set_fail,
    "set_proxy": cmd_set_proxy,
    "ext_status": cmd_ext_status,
    "list": cmd_list, "get": cmd_get,
}


def main():
    if len(sys.argv) < 2 or sys.argv[1] not in CMDS:
        print(json.dumps({"error": "usage: pool.py {%s}" % "|".join(CMDS)}))
        return 2
    try:
        return CMDS[sys.argv[1]]() or 0
    except Exception as e:
        print(json.dumps({"error": "%s: %s" % (type(e).__name__, e)}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
