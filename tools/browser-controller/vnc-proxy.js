#!/usr/bin/env node
/**
 * gw2a VNC path proxy
 * -------------------
 * fnOS 的 nginx 里 `/chromium/` 路由被系统守护进程强制写死为
 * `proxy_pass http://127.0.0.1:3000;`（改完几十秒内被回滚），并且会把完整
 * URI（含 /chromium/ 前缀）原样转发。
 *
 * 所以我们不去改 nginx，而是在 127.0.0.1:3000 上放一个自己的反向代理：
 *   /chromium/...  ->  http://127.0.0.1:16100/...   （剥掉前缀）
 *   同时转发 WebSocket Upgrade（KasmVNC 的 noVNC 依赖）
 *
 * 这样 http://<nas>:5666/chromium/ 就能直接打开我们的 Xkasmvnc 桌面。
 */
'use strict';

const http = require('http');

const LISTEN_HOST = process.env.GW2A_VNC_PROXY_HOST || '127.0.0.1';
const LISTEN_PORT = parseInt(process.env.GW2A_VNC_PROXY_PORT || '3000', 10);
const UPSTREAM_HOST = process.env.GW2A_VNC_UPSTREAM_HOST || '127.0.0.1';
const UPSTREAM_PORT = parseInt(process.env.GW2A_VNC_UPSTREAM_PORT || '16100', 10);
const PREFIX = process.env.GW2A_VNC_PREFIX || '/chromium';

function stripPrefix(url) {
  let u = url || '/';
  if (u === PREFIX) u = '/';
  else if (u.startsWith(PREFIX + '/')) u = u.slice(PREFIX.length);
  if (!u.startsWith('/')) u = '/' + u;
  return u;
}

// KasmVNC 4.0 的 noVNC 前端默认 path 是 "websockify"，但服务端只接受 /api/ws。
// 这里做一层路径映射，避免改前端资源。
const PATH_MAP = [
  [/^\/websockify(\?.*)?$/, (m) => '/api/ws' + (m[1] || '')],
  [/^\/(?:vnc\/ws|ws)(\?.*)?$/, (m) => '/api/ws' + (m[1] || '')],
];

function mapUpstreamPath(u) {
  for (const pair of PATH_MAP) {
    const m = u.match(pair[0]);
    if (m) return pair[1](m);
  }
  return u;
}

function upstreamPath(url) {
  return mapUpstreamPath(stripPrefix(url));
}

function proxyHeaders(headers) {
  const h = Object.assign({}, headers);
  h.host = UPSTREAM_HOST + ':' + UPSTREAM_PORT;
  // KasmVNC 会按 Origin 做校验，统一改成上游地址
  if (h.origin) h.origin = 'http://' + UPSTREAM_HOST + ':' + UPSTREAM_PORT;
  delete h['x-forwarded-for'];
  delete h['x-real-ip'];
  // 不做压缩，保证 HTML/JS 响应体可以被安全改写
  delete h['accept-encoding'];
  return h;
}

// noVNC 计算 WS 地址时是 root-anchored（ws://host/<path>），不带当前目录前缀，
// 因此页面里的 path 必须写成 "chromium/websockify"，否则会请求 /websockify 而 404。
// 这里对 HTML / JS 响应体做一次字符串替换，避免改上游静态资源。
function rewriteBody(buf, ct) {
  if (!/text\/html|javascript|application\/json/i.test(ct || '')) return buf;
  const s = buf.toString('utf8');
  if (s.indexOf('websockify') < 0) return buf;
  const out = s.split('"websockify"').join('"chromium/websockify"');
  return Buffer.from(out, 'utf8');
}

const server = http.createServer((req, res) => {
  const path = upstreamPath(req.url);
  const upReq = http.request({
    host: UPSTREAM_HOST,
    port: UPSTREAM_PORT,
    path: path,
    method: req.method,
    headers: proxyHeaders(req.headers),
  }, (upRes) => {
    const h = Object.assign({}, upRes.headers);
    // 上游若做了绝对重定向，改写回带前缀的地址
    if (h.location && h.location.startsWith('/')) h.location = PREFIX + h.location;
    const ct = h['content-type'] || '';
    if (/text\/html|javascript/i.test(ct)) {
      const chunks = [];
      upRes.on('data', (c) => chunks.push(c));
      upRes.on('end', () => {
        const body = rewriteBody(Buffer.concat(chunks), ct);
        delete h['content-length'];
        h['content-length'] = String(body.length);
        res.writeHead(upRes.statusCode || 502, h);
        res.end(body);
      });
      upRes.on('error', () => { try { res.destroy(); } catch (e) {} });
      return;
    }
    res.writeHead(upRes.statusCode || 502, h);
    upRes.pipe(res);
  });
  upReq.on('error', (e) => {
    if (!res.headersSent) {
      res.writeHead(502, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end('gw2a vnc proxy error: ' + e.message + '\n');
    } else {
      try { res.destroy(); } catch (e2) {}
    }
  });
  req.pipe(upReq);
});

server.on('upgrade', (req, socket, head) => {
  const path = upstreamPath(req.url);
  const upReq = http.request({
    host: UPSTREAM_HOST,
    port: UPSTREAM_PORT,
    path: path,
    method: 'GET',
    headers: proxyHeaders(req.headers),
  });
  upReq.on('upgrade', (upRes, upSocket, upHead) => {
    let out = 'HTTP/1.1 ' + (upRes.statusCode || 101) + ' ' + (upRes.statusMessage || 'Switching Protocols') + '\r\n';
    for (const k of Object.keys(upRes.headers)) {
      const v = upRes.headers[k];
      if (Array.isArray(v)) v.forEach((x) => { out += k + ': ' + x + '\r\n'; });
      else out += k + ': ' + v + '\r\n';
    }
    out += '\r\n';
    try { socket.write(out); } catch (e) {}
    if (upHead && upHead.length) upSocket.unshift(upHead);
    if (head && head.length) upReq.socket.unshift(head);
    upSocket.pipe(socket);
    socket.pipe(upSocket);
    upSocket.on('error', () => { try { socket.destroy(); } catch (e) {} });
    socket.on('error', () => { try { upSocket.destroy(); } catch (e) {} });
  });
  upReq.on('error', () => {
    try { socket.end('HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n'); } catch (e) {}
    try { socket.destroy(); } catch (e) {}
  });
  upReq.end();
});

server.listen(LISTEN_PORT, LISTEN_HOST, () => {
  console.log('[gw2a-vnc-proxy] ' + LISTEN_HOST + ':' + LISTEN_PORT + PREFIX + '/ -> http://' +
    UPSTREAM_HOST + ':' + UPSTREAM_PORT + '/');
});
