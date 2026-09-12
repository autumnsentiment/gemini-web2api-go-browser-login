#!/usr/bin/env python3
"""gemini-web2api cookie 池读写助手（宿主机侧，直接操作容器共享的 SQLite）。

子命令：
  upsert   从 stdin 读 JSON {profile,label,cookie,note,source}，按 (source,profile)
           找到既有账号则更新 cookie（保号），否则新增；同时删除该 profile 的
           其它历史记录（只留最新一条）。输出 JSON {id, action, removed}
  set_ok   从 stdin 读 JSON {id}：标记账号可用（status=enabled, last_ok_at=now, fail_count=0）
  set_fail 从 stdin 读 JSON {id,error}：标记账号失败（last_error, fail_count+1）
  set_proxy 从 stdin 读 JSON {id,proxy_id}：把账号绑定到指定出口（0=直连）
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
    if not cookie:
        print(json.dumps({"error": "empty cookie"}))
        return 2
    con = connect()
    try:
        row = con.execute(
            "SELECT id FROM accounts WHERE source=? AND profile=? "
            "ORDER BY id DESC LIMIT 1", (source, profile)).fetchone()
        t = now()
        if row:
            aid = row[0]
            con.execute(
                "UPDATE accounts SET cookie=?, label=?, note=?, status='enabled', "
                "last_error='', last_ok_at=?, fail_count=0 WHERE id=?",
                (cookie, label, note, t, aid))
            action = "updated"
        else:
            cur = con.execute(
                "INSERT INTO accounts (label, cookie, status, note, created_at, "
                "last_used_at, last_ok_at, last_error, fail_count, proxy_id, source, profile) "
                "VALUES (?,?,'enabled',?,?,0,?,'',0,0,?,?)",
                (label, cookie, note, t, t, source, profile))
            aid = cur.lastrowid
            action = "inserted"
        # 清掉同一 profile 的其它历史记录：每次抓取只保留最新一条，
        # 否则池子里会堆一串同账号的旧 cookie（有的已失效），轮询到就报错。
        removed = con.execute(
            "DELETE FROM accounts WHERE source=? AND profile=? AND id<>?",
            (source, profile, aid)).rowcount
        print(json.dumps({"id": aid, "action": action, "profile": profile,
                          "cookie_len": len(cookie), "ts": t, "removed": removed}))
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
