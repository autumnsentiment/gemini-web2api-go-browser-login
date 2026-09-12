#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""gw2a-cookie-queue —— 队列模式的 cookie 抓取调度器（替代 systemd timer）。

统一调度所有「抓 cookie」触发源，保证同一时刻只有一个抓取任务在跑
（避免浏览器被并发拉起、多个任务互相抢同一个 profile）：

  触发源 1（定时）：每 GW2A_SCHED_SEC（默认 1800 秒 = 30 分钟）入队一个 sched 任务。
  触发源 2（502）：监听容器 requests 表，累计出现 GW2A_502_THRESHOLD（默认 2）
                  个 502 就入队一个 error 任务，并 **重置定时器**（重新计时 30 分钟）。

队列 FIFO、串行执行：每个任务调用 refresh.py 刷新 Gemini 页面重新抓 cookie 入池。
执行期间新来的触发源继续入队，不打断当前任务，执行完接着跑下一个。

状态文件：/var/lib/gw2a-cookie-sync/queue-state.json（持久化，重启不丢）
日志：stdout（systemd 收集到 /var/log/gw2a-cookie-queue.log）
"""
import json
import os
import sqlite3
import subprocess
import sys
import time
import urllib.request

DB_PATH = os.environ.get(
    "GW2A_DB",
    "/vol2/docker/volumes/gemini-web2api-go_gw2a-data/_data/gemini.db")
STATE_DIR = os.environ.get("GW2A_STATE_DIR", "/var/lib/gw2a-cookie-sync")
STATE_FILE = os.path.join(STATE_DIR, "queue-state.json")
PROFILE = os.environ.get("GW2A_PROFILE", "acct1")
REFRESH_PY = os.environ.get("GW2A_REFRESH_PY", "/opt/gw2a-cookie-sync/refresh.py")
PY = os.environ.get("GW2A_PYTHON", "/opt/gw2a-cookie-sync/venv/bin/python")

POLL_SEC = float(os.environ.get("GW2A_POLL_SEC", "5") or 5)
SCHED_SEC = int(os.environ.get("GW2A_SCHED_SEC", "1800") or 1800)     # 定时周期（秒）
ERR_THRESHOLD = int(os.environ.get("GW2A_502_THRESHOLD", "2") or 2)   # 累计几个 502 触发
# 502 触发后的防抖冷却：只需避开「同一批失败反复触发」。用户要求「立刻」，
# 所以默认只留 60s；真正防连环刷新的是 refresh.py 里的 MIN_GAP_SEC。
ERR_COOLDOWN_SEC = int(os.environ.get("GW2A_502_COOLDOWN_SEC", "60") or 60)
JOB_TIMEOUT = int(os.environ.get("GW2A_JOB_TIMEOUT", "480") or 480)

# 只看「cookie 失效」这类 502 更精准；默认 all 表示任何 502 都算（用户要求）。
ERR_MODE = (os.environ.get("GW2A_502_MODE", "all") or "all").strip().lower()
COOKIE_ERR_HINTS = ("cookie 池", "no SNlM0e", "cookie expired", "HTTP 302",
                    "EOF", "redirect", "登录")


def log(*a):
    print("[queue] " + time.strftime("%F %T"), *a, flush=True)


# ---------------------------------------------------------------- state

def load_state():
    try:
        with open(STATE_FILE, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return {}


def save_state(**kw):
    st = load_state()
    st.update(kw)
    try:
        os.makedirs(STATE_DIR, exist_ok=True)
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(st, f, ensure_ascii=False, indent=1)
        os.replace(tmp, STATE_FILE)
    except Exception as e:
        log("写状态失败（忽略）:", e)
    return st


# ---------------------------------------------------------------- db

def connect_ro():
    con = sqlite3.connect("file:%s?mode=ro" % DB_PATH, uri=True, timeout=20)
    con.row_factory = sqlite3.Row
    return con


def max_request_id():
    try:
        con = connect_ro()
        try:
            r = con.execute("SELECT COALESCE(MAX(id),0) AS m FROM requests").fetchone()
            return int(r["m"])
        finally:
            con.close()
    except Exception as e:
        log("读 requests 失败（忽略）:", e)
        return None


def poll_502(since_id):
    """返回 (新的最大 id, 502 明细列表)。"""
    try:
        con = connect_ro()
        try:
            rows = list(con.execute(
                "SELECT id, ts, status, COALESCE(error,'') AS error, "
                "       COALESCE(account_label,'') AS acct "
                "FROM requests WHERE id > ? ORDER BY id", (since_id,)))
            mx = since_id
            hits = []
            for r in rows:
                mx = max(mx, int(r["id"]))
                if int(r["status"]) == 502:
                    err = r["error"] or ""
                    if ERR_MODE == "cookie" and not any(h in err for h in COOKIE_ERR_HINTS):
                        continue
                    hits.append({"id": int(r["id"]), "error": err[:80],
                                 "acct": r["acct"]})
            return mx, hits
        finally:
            con.close()
    except Exception as e:
        log("轮询 requests 失败（忽略）:", e)
        return since_id, []


# ------------------------------------------------------- browser keepalive

CTRL = os.environ.get("GW2A_CTRL_URL", "http://127.0.0.1:9280").rstrip("/")
KEEPALIVE_SEC = int(os.environ.get("GW2A_BROWSER_KEEPALIVE_SEC", "60") or 60)


def ctrl_json(path, method="GET", body=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        CTRL + path, data=data, method=method,
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


_browser_missing_since = 0.0


def ensure_browser_alive():
    """浏览器必须常驻：Google 的 RotateCookiesPage iframe 只在页面存活时
    才会把 __Secure-1PSIDTS 轮转下去。浏览器一关，会话立刻停止轮转，随后
    服务端把会话判成匿名（/app 无 SNlM0e），池子里的 cookie 全变废。

    这里每分钟检查一次，不在就拉起；连续拉起失败只记日志，不中断队列。
    """
    global _browser_missing_since
    try:
        d = ctrl_json("/profiles")
        names = [p.get("name") for p in (d.get("profiles") or [])]
        if PROFILE in names:
            if _browser_missing_since:
                log("浏览器已恢复（profile=%s）" % PROFILE)
                _browser_missing_since = 0.0
            return True
    except Exception as e:
        log("保活：查询控制器失败（忽略）:", e)
        return False
    if not _browser_missing_since:
        _browser_missing_since = time.time()
        log("保活：profile=%s 不在运行，尝试拉起" % PROFILE)
    try:
        r = ctrl_json("/profiles", "POST", {"name": PROFILE})
        log("保活：已拉起 profile=%s port=%s pid=%s"
            % (PROFILE, r.get("port"), r.get("pid")))
    except Exception as e:
        log("保活：拉起失败（下次再试）:", e)
    return False


# ---------------------------------------------------------------- job

def run_refresh(reason):
    """跑一轮 refresh.py（阻塞）。返回 (ok, 摘要字符串)。"""
    # 502 触发 → 强制刷新（但仍受 refresh.py 里 MIN_GAP_SEC 最小间隔约束，
    #            避免连环刷新触发风控）；
    # 定时触发 → 常规路径（冷却窗口内能取到就不刷，取不到再刷）。
    if reason == "error":
        cmd = [PY, REFRESH_PY, PROFILE, "--force-refresh", "--no-stop"]
    else:
        cmd = [PY, REFRESH_PY, PROFILE, "--wait-cooldown", "--no-stop"]
    t0 = time.time()
    log("执行抓取任务 reason=%s ..." % reason)
    try:
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                           timeout=JOB_TIMEOUT)
        out = p.stdout.decode("utf-8", "replace")
        took = time.time() - t0
        # 日志太长就只留最后一行 JSON
        last = ""
        for line in out.strip().splitlines():
            if line.startswith("{") and '"profile"' in line:
                last = line
        try:
            d = json.loads(last) if last else {}
        except Exception:
            d = {}
        if p.returncode == 0 and d.get("ok"):
            ck = d.get("check") or {}
            detail = "ok id=%s cookie=%sB refresh=%s %s" % (
                d.get("id"), d.get("cookie_len"), d.get("refreshes_count"),
                ("校验✓" if ck.get("ok") else ("校验-" + str(ck.get("detail"))[:40]
                                             if ck else "")))
            log("任务完成 %.0fs %s" % (took, detail))
            return True, detail
        err = d.get("error") or ("exit=%s" % p.returncode)
        tail = "\n".join(out.strip().splitlines()[-3:])
        log("任务失败 %.0fs: %s" % (took, err))
        log("  尾部日志: " + tail.replace("\n", " | ")[:300])
        return False, err
    except subprocess.TimeoutExpired:
        log("任务超时（>%ds），已中止" % JOB_TIMEOUT)
        return False, "timeout"
    except Exception as e:
        log("任务异常:", e)
        return False, str(e)


# ---------------------------------------------------------------- main

def main():
    os.makedirs(STATE_DIR, exist_ok=True)
    st = load_state()
    queue = list(st.get("queue") or [])
    last_id = int(st.get("lastRequestId") or 0)
    pending_502 = int(st.get("pending502") or 0)
    next_sched = float(st.get("nextSchedAt") or 0)
    err_until = float(st.get("errCooldownUntil") or 0)
    seq = int(st.get("seq") or 0)
    last_keepalive_check = 0.0

    if not last_id:
        last_id = max_request_id() or 0
        log("首次启动，从 request id=%d 之后开始监听" % last_id)

    # 启动时如果已经有积压的 502，立即让本轮触发（不要让重启前的冷却把
    # 它继续压着 —— 那会造成「明明超过阈值却迟迟不重抓」）。
    if pending_502 >= ERR_THRESHOLD:
        err_until = 0

    now = time.time()
    if next_sched <= now:
        next_sched = now + SCHED_SEC
    log("队列调度器启动：定时=%ds 502阈值=%d 模式=%s 下次定时=%s"
        % (SCHED_SEC, ERR_THRESHOLD, ERR_MODE,
           time.strftime("%H:%M:%S", time.localtime(next_sched))))

    def persist():
        save_state(queue=queue, lastRequestId=last_id, pending502=pending_502,
                   nextSchedAt=next_sched, errCooldownUntil=err_until, seq=seq)

    persist()

    # 启动即确认浏览器在跑（常驻是 cookie 轮转的前提）
    ensure_browser_alive()
    last_keepalive_check = time.time()

    while True:
        try:
            now = time.time()

            # ── 触发源 1：502 检测 ──────────────────────────────────
            mx, hits = poll_502(last_id)
            if mx != last_id:
                last_id = mx
                if hits:
                    pending_502 += len(hits)
                    for h in hits:
                        log("检测到 502 #%d acct=%s err=%s"
                            % (h["id"], h["acct"] or "-", h["error"]))
                persist()   # 立即落盘，重启不丢计数

            if pending_502 >= ERR_THRESHOLD and now >= err_until:
                seq += 1
                queue.append({"seq": seq, "type": "error", "at": now,
                              "detail": "累计 %d 个 502" % pending_502})
                log("★ 502 达到阈值（%d 个）→ 入队重抓，并重置定时器"
                    % pending_502)
                pending_502 = 0
                next_sched = now + SCHED_SEC        # ← 触发重抓则重新计时
                err_until = now + ERR_COOLDOWN_SEC
                persist()

            # ── 触发源 2：定时 ─────────────────────────────────────
            if now >= next_sched:
                seq += 1
                queue.append({"seq": seq, "type": "sched", "at": now,
                              "detail": "定时 %d 分钟" % (SCHED_SEC // 60)})
                next_sched = now + SCHED_SEC
                log("定时到点 → 入队（队列长度 %d）" % len(queue))
                persist()

            # ── 触发源 3：浏览器保活（常驻是 cookie 轮转的前提）──────
            if now - last_keepalive_check >= KEEPALIVE_SEC:
                last_keepalive_check = now
                ensure_browser_alive()

            # ── 执行队列（串行，一次一个）───────────────────────────
            if queue:
                job = queue.pop(0)
                persist()
                ok, detail = run_refresh(job["type"])
                job["doneAt"] = time.time()
                job["ok"] = ok
                job["result"] = detail
                hist = list(load_state().get("history") or [])
                hist.append(job)
                save_state(history=hist[-50:], lastJob=job)
                persist()

            time.sleep(POLL_SEC)
        except KeyboardInterrupt:
            log("收到中断，退出")
            persist()
            return 0
        except Exception as e:
            log("主循环异常（继续）:", e)
            time.sleep(POLL_SEC)


if __name__ == "__main__":
    sys.exit(main())
