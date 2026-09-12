"""Minimal Chrome DevTools Protocol client (sync, websocket-client based)."""
import itertools
import json
import threading
import time

import websocket


class CDPError(Exception):
    pass


class CDP(object):
    def __init__(self, ws_url, timeout=30):
        self.ws_url = ws_url
        self.ws = websocket.create_connection(
            ws_url, timeout=timeout, max_size=256 * 1024 * 1024,
            suppress_origin=True, enable_multithread=True)
        self._ids = itertools.count(1)
        self._lock = threading.Lock()
        self._pending = {}
        self._closed = False
        self._events = []
        self._evt_lock = threading.Lock()
        self._handlers = []
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._reader.start()

    # -- internals ---------------------------------------------------------
    def _read_loop(self):
        while not self._closed:
            try:
                raw = self.ws.recv()
            except Exception:
                break
            if not raw:
                continue
            try:
                msg = json.loads(raw)
            except Exception:
                continue
            mid = msg.get("id")
            if mid is not None and ("result" in msg or "error" in msg):
                with self._lock:
                    box = self._pending.pop(mid, None)
                if box is not None:
                    box["msg"] = msg
                    box["ev"].set()
                continue
            method = msg.get("method")
            if method:
                with self._evt_lock:
                    self._events.append(msg)
                    if len(self._events) > 5000:
                        del self._events[:2500]
                for h in list(self._handlers):
                    try:
                        h(msg)
                    except Exception:
                        pass
        # unblock everything on close
        with self._lock:
            pending = list(self._pending.values())
            self._pending.clear()
        for box in pending:
            box["msg"] = {"error": {"message": "connection closed"}}
            box["ev"].set()

    def send(self, method, params=None, session_id=None, timeout=30):
        mid = next(self._ids)
        payload = {"id": mid, "method": method}
        if params:
            payload["params"] = params
        if session_id:
            payload["sessionId"] = session_id
        box = {"ev": threading.Event(), "msg": None}
        with self._lock:
            self._pending[mid] = box
        self.ws.send(json.dumps(payload))
        if not box["ev"].wait(timeout):
            with self._lock:
                self._pending.pop(mid, None)
            raise CDPError("timeout waiting for %s" % method)
        msg = box["msg"]
        if "error" in msg:
            raise CDPError("%s: %s" % (method, msg["error"].get("message")))
        return msg.get("result", {})

    def add_handler(self, fn):
        self._handlers.append(fn)

    def wait_event(self, method, predicate=None, timeout=30, since=None):
        deadline = time.time() + timeout
        idx = 0 if since is None else since
        while time.time() < deadline:
            with self._evt_lock:
                evs = self._events[idx:]
                idx = len(self._events)
            for ev in evs:
                if ev.get("method") == method and (predicate is None or predicate(ev)):
                    return ev
            time.sleep(0.05)
        return None

    def close(self):
        self._closed = True
        try:
            self.ws.close()
        except Exception:
            pass


def http_json(url, timeout=10):
    import urllib.request
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8", "replace"))
