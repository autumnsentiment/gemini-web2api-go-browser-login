#!/usr/bin/env node
/**
 * gemini-web2api-go 配套的 Chromium Profile 控制器（fnOS 宿主版）
 * ----------------------------------------------------------
 * 运行位置：fnOS 宿主机（root），依赖系统 /usr/bin/node (v18)。
 * 职责：按需启动/停止「账号隔离」的独立 Chromium 实例（用 fygo-browser 自带的
 *       Chromium 152 + Xkasmvnc :13 桌面），每个实例用独立的 --user-data-dir
 *       存放登录态、独立的 CDP 调试端口，供 gemini-web2api-go 通过 CDP
 *       导航 + 抓 cookie。
 *
 * 关键点：
 *  - Chromium 以 fygo-browser 用户运行（preflight 拒绝 root 跑 Chromium）。
 *  - CDP 端口只绑 127.0.0.1，因此本控制器提供 HTTP/WS 反向隧道：
 *      HTTP: /cdp/<port>/json/list|version|new?...  → 127.0.0.1:<port>/...
 *      WS:   Upgrade /cdp/<port>/devtools/page/<id> → 127.0.0.1:<port>/...
 *    gemini-web2api（docker 容器）经 http://<docker网关>:9280/cdp/<port>/... 访问。
 *  - 本服务监听 0.0.0.0:9280。
 *
 * 环境变量：
 *   GW2A_CTRL_PORT     默认 9280
 *   GW2A_PROFILE_BASE  默认 /vol2/@appdata/gw2a-browser/profiles
 *   GW2A_CDP_BASE      默认 9300
 *   GW2A_DISPLAY       默认 :13
 *   GW2A_GPU_DISPLAY   默认空；设为 ':1' 后新 profile 默认用 GPU Xorg（virgl）
 *   GW2A_RUN_USER      默认 fygo-browser
 *   GW2A_IDLE_STOP_SEC 默认 180；profile 空闲这么久没有 CDP 流量就自动休眠
 *                      （优雅 SIGTERM，cookie 正常落盘）。0 = 关闭自动休眠。
 *                      抓取前由调用方 POST /profiles 唤醒，抓完自动睡 —— 平时
 *                      不占 CPU/内存。用户要手动登录时先 POST /profiles/<n>/hold。
 */
'use strict';

const http = require('http');
const net = require('net');
const { spawn, execSync, execFile } = require('child_process');
const fs = require('fs');
const path = require('path');

const PORT = parseInt(process.env.GW2A_CTRL_PORT || '9280', 10);
const PROFILE_BASE = process.env.GW2A_PROFILE_BASE || '/vol2/@appdata/gw2a-browser/profiles';
const CDP_BASE = parseInt(process.env.GW2A_CDP_BASE || '9300', 10);
const DISPLAY = process.env.GW2A_DISPLAY || ':13';
// GPU 显示：设为 GW2A_GPU_DISPLAY（如 ':1'）后，新 profile 默认跑在该 Xorg 上
//（modesetting+glamor+DRI3，virtio-gpu virgl 硬件 GL）。Xkasmvnc :13 无 DRI3，
// 只能 llvmpipe 软渲染。按次覆盖：POST /profiles {"display": ":13"}。
const GPU_DISPLAY = process.env.GW2A_GPU_DISPLAY || '';
const RUN_USER = process.env.GW2A_RUN_USER || 'fygo-browser';

const RT = '/vol2/@appcenter/fygo-browser/app/vendor/runtime';
const CHROMIUM = RT + '/usr/lib/chromium/chromium';
const BROWSER_HOME = '/vol2/@appdata/gw2a-browser/home';
const BROWSER_RUN = '/vol2/@appdata/gw2a-browser/run';
const LD_LIB = RT + '/usr/lib/x86_64-linux-gnu:' + RT + '/usr/lib:' + RT + '/usr/lib/chromium';

// 启动页：直接开 Gemini（而不是 about:blank）。
//
// ★ 2026-09-18 ★ 配合「按需唤醒」：浏览器平时休眠，被唤醒就是为了抓 cookie，
// 启动页直接落在 gemini.google.com 的话，抓取脚本一上来就能走「只读抓取」
// （不导航、不刷新页面），省掉一次整页加载 —— 既快又少一次风控暴露。
const START_URL = process.env.GW2A_START_URL || 'https://gemini.google.com/';

const CHROME_FLAGS = [
  '--no-first-run',
  '--no-default-browser-check',
  '--no-sandbox',
  '--test-type',
  '--disable-infobars',
  '--ignore-gpu-blocklist',
  '--password-store=basic',
  '--use-mock-keychain',
  '--disable-dev-shm-usage',
  '--disable-features=Translate,MediaRouter,OptimizationHints',
  '--disable-background-networking',
  '--disable-sync',
  '--no-service-autorun',
  '--remote-allow-origins=*',
  // ── 资源限制（2026-09-18 用户要求：平时别常驻一堆浏览器内核进程）──────
  // renderer 数量上限：页面数量受控后 2 个够用（gemini 页 + 空白页），
  // 历史遗留的僵尸标签页不会再各占一个 renderer。
  '--renderer-process-limit=2',
  // 用不到的后台服务全部关掉：每个都是常驻进程，白吃 CPU 与内存。
  '--disable-component-update',
  '--disable-domain-reliability',
  '--disable-client-side-phishing-detection',
  '--disable-crash-reporter',
  '--no-crashpad',
  '--disable-breakpad',
  '--metrics-recording-only',
  '--mute-audio',
  // 显式保留「后台标签页降频」：默认就是开的，这里写明是为了防止
  // 后续有人加上 --disable-background-timer-throttling 把 CPU 吃回去。
];

// Browser egress proxy (empty = direct). On dual-stack networks an
// inconsistent v4/v6 egress triggers Google's anti-bot check; use the
// same egress as the cookie pool.
const BROWSER_PROXY = process.env.GW2A_BROWSER_PROXY || '';
if (BROWSER_PROXY) {
  CHROME_FLAGS.push('--proxy-server=' + BROWSER_PROXY);
  // Loopback must stay direct, else the extension cannot reach the
  // controller. NB: Chromium only bypasses IP literals via CIDR or
  // <local>; a bare 127.0.0.1 is ignored and the request would go
  // through the proxy, breaking the extension's push.
  CHROME_FLAGS.push('--proxy-bypass-list=localhost;127.0.0.1/32;[::1];<local>');
}

// ── Cookie 池写入（扩展 → 控制器 → 容器 SQLite）──────────────────────────
const POOL_PY = process.env.GW2A_POOL_PY || '/opt/gw2a-cookie-sync/pool.py';
const PY = process.env.GW2A_PYTHON || '/usr/bin/python3';
const GW2A_API = (process.env.GW2A_API || 'http://127.0.0.1:8083').replace(/\/+$/, '');
const GW2A_ADMIN_TOKEN = process.env.GW2A_ADMIN_TOKEN || '';
// 浏览器所用出口在 gemini-web2api 代理表里的 id（0 = 自动按 URL 匹配）
const BROWSER_PROXY_ID = parseInt(process.env.GW2A_BROWSER_PROXY_ID || '0', 10) || 0;
const SYNC_TOKEN = process.env.GW2A_SYNC_TOKEN || '';
const EXT_DIR = process.env.GW2A_EXT_DIR || '/opt/gw2a-cookie-sync/ext';
const EXT_ID = process.env.GW2A_EXT_ID || '';
const SYNC_LOG = '/var/log/gw2a-cookie-sync.log';

function syncLog(msg) {
  const line = new Date().toISOString() + ' ' + msg + '\n';
  try { fs.appendFileSync(SYNC_LOG, line); } catch (e) {}
  console.log('[gw2a-cookie-sync]', msg);
}

// 仅允许本机/内网来源写入 cookie 池（防止局域网内他人注入账号）
function isPrivateIp(ip) {
  if (!ip) return false;
  let a = String(ip);
  if (a.indexOf('::ffff:') === 0) a = a.slice(7);
  if (a === '::1' || a === '127.0.0.1') return true;
  const m = a.match(/^(\d+)\.(\d+)\.(\d+)\.(\d+)$/);
  if (!m) return false;
  const p = m.slice(1).map(Number);
  if (p[0] === 10) return true;
  if (p[0] === 192 && p[1] === 168) return true;
  if (p[0] === 172 && p[1] >= 16 && p[1] <= 31) return true;
  return false;
}

// 调用 pool.py 子命令，stdin 传 JSON，stdout 取 JSON
function runPool(sub, obj, timeoutMs) {
  return new Promise((resolve) => {
    const child = execFile(PY, [POOL_PY, sub], {
      timeout: timeoutMs || 20000, maxBuffer: 8 * 1024 * 1024,
    }, (err, stdout, stderr) => {
      if (err && !stdout) {
        return resolve({ error: (stderr || err.message || 'pool.py failed').trim() });
      }
      try { resolve(JSON.parse(stdout)); }
      catch (e) { resolve({ error: 'bad pool output: ' + String(stdout).slice(0, 200) }); }
    });
    try { child.stdin.end(JSON.stringify(obj || {})); } catch (e) {}
  });
}

// 极简 HTTP JSON 请求
function httpJSON(opts, bodyObj) {
  return new Promise((resolve) => {
    const payload = bodyObj ? JSON.stringify(bodyObj) : null;
    const headers = Object.assign({}, opts.headers || {});
    if (payload) {
      headers['Content-Type'] = 'application/json';
      headers['Content-Length'] = Buffer.byteLength(payload);
    }
    const req = http.request({
      host: opts.host, port: opts.port, path: opts.path,
      method: opts.method || 'GET', headers: headers,
    }, (res) => {
      let data = '';
      res.on('data', (c) => { data += c; });
      res.on('end', () => {
        let json = null;
        try { json = JSON.parse(data); } catch (e) {}
        resolve({ status: res.statusCode, headers: res.headers, json: json, raw: data });
      });
    });
    req.on('error', (e) => resolve({ error: e.message }));
    req.setTimeout(30000, () => { try { req.destroy(); } catch (e) {} resolve({ error: 'timeout' }); });
    if (payload) req.write(payload);
    req.end();
  });
}

function apiHostPort() {
  const u = GW2A_API.replace(/^https?:\/\//, '').split('/')[0];
  const i = u.lastIndexOf(':');
  return { host: i > 0 ? u.slice(0, i) : u, port: i > 0 ? parseInt(u.slice(i + 1), 10) : 80 };
}

// 登录 admin API，返回 cookie 头；失败返回 null
async function adminLogin() {
  if (!GW2A_ADMIN_TOKEN) return null;
  const hp = apiHostPort();
  const r = await httpJSON({ host: hp.host, port: hp.port, path: '/admin/api/login', method: 'POST' },
    { token: GW2A_ADMIN_TOKEN });
  if (!r || !r.json || !r.json.ok) return null;
  const sc = (r.headers && r.headers['set-cookie']) || [];
  const jar = sc.map((c) => String(c).split(';')[0]).join('; ');
  return jar || null;
}

// 让 gemini-web2api 对刚入池的账号做一次真实可用性检查（SNlM0e 探测）
// 解析浏览器出口在 gemini-web2api 代理表里的 id。
// 浏览器与 cookie 池必须走同一出口：出口不一致会被上游当可疑流量，
// 已入池的会话 cookie 也可能立刻失效。匹配不到就返回 0（不绑定）。
let _proxyIdCache = { at: 0, id: 0 };
async function resolveProxyId() {
  if (BROWSER_PROXY_ID > 0) return BROWSER_PROXY_ID;
  if (!GW2A_ADMIN_TOKEN || !BROWSER_PROXY) return 0;
  if (Date.now() - _proxyIdCache.at < 300000) return _proxyIdCache.id;
  try {
    const jar = await adminLogin();
    if (!jar) return 0;
    const hp = apiHostPort();
    const r = await httpJSON({ host: hp.host, port: hp.port, path: '/admin/api/proxies',
                               method: 'GET', headers: { Cookie: jar } });
    const items = (r && r.json && r.json.items) || [];
    const want = BROWSER_PROXY.replace(/\/+$/, '');
    const hit = items.find((x) => String(x.url || '').replace(/\/+$/, '') === want);
    _proxyIdCache = { at: Date.now(), id: hit ? hit.id : 0 };
    if (!hit) syncLog('警告：代理表里找不到 ' + want + '，入池账号将不绑定出口');
    return _proxyIdCache.id;
  } catch (e) { return 0; }
}

async function apiCheck(id, jar) {
  if (!jar) return null;
  const hp = apiHostPort();
  const r = await httpJSON({
    host: hp.host, port: hp.port,
    path: '/admin/api/cookies/' + encodeURIComponent(id) + '/check',
    method: 'POST', headers: { Cookie: jar },
  });
  return r && r.json ? r.json : null;
}

// ── 会话指纹：核心登录凭据不变 = 还是同一个会话，无需重复打上游校验 ──────
// SIDCC / __Secure-1PSIDCC 这类轮换 cookie 每分钟都变，不能进指纹，否则
// 每次推送都被判成「新会话」，反复消耗上游配额。
const _checkCache = new Map();   // profile -> { fp, check, at }
// 缓存 TTL：即使 SID 没变，也至多隔 10 分钟重新打一次上游校验。
// 否则出口断了时，旧 cookie 会一直被判成「同会话可用」，把
// 真实的网络故障掩盖掉，排查时会误导。
const CHECK_CACHE_TTL_MS = 10 * 60 * 1000;

function sessionFingerprint(cookie) {
  const want = ['SID', 'SAPISID', '__Secure-1PSID', '__Secure-3PSID', 'LSID'];
  const out = [];
  for (const part of String(cookie || '').split(';')) {
    const i = part.indexOf('=');
    if (i < 0) continue;
    const k = part.slice(0, i).trim();
    if (want.indexOf(k) >= 0) out.push(k + '=' + part.slice(i + 1).trim());
  }
  out.sort();
  return require('crypto').createHash('sha1').update(out.join('|')).digest('hex');
}

async function handleCookieSync(body, clientIp) {
  if (SYNC_TOKEN) {
    const got = body && body._token;
    if (got !== SYNC_TOKEN) return { error: 'unauthorized', code: 401 };
  } else if (!isPrivateIp(clientIp)) {
    syncLog('拒绝非内网来源 ' + clientIp);
    return { error: 'forbidden (non-private source; set GW2A_SYNC_TOKEN)', code: 403 };
  }

  const cookie = (body && body.cookie || '').trim();
  const profile = (body && body.profile || 'browser1').trim();
  if (!cookie) return { error: 'cookie 为空' };
  if (!/SAPISID=/.test(cookie)) return { error: 'cookie 缺少 SAPISID，未登录或抓取不完整' };

  // ★ 扩展 v1.1.0 起会上报真实登录态（以页面 SNlM0e 为准）。
  // 会话被 Google 判匿名时，cookie 名字仍然齐、有效期还很长，但服务端不认；
  // 这种废 cookie 一旦写进池子，gemini-web2api 拿到就 302（重定向登录页）→ 请求
  // 502 → 触发重抓 → 又抓到同一份废 cookie，形成死循环。
  // 所以这里直接拒收，让上游去看门狗/扩展去重登。
  if (body && body.logged_in === false) {
    const why = '会话已判匿名（扩展探针：' + (body.login_source || 'page') + '），拒绝入池';
    syncLog('拒收 profile=' + profile + '：' + why);
    return { ok: false, error: why, rejected: true, login_source: body.login_source || '' };
  }

  const label = (body && body.label) || ('browser:' + profile);
  const note = (body && body.note) || ('auto by extension @ ' + new Date().toISOString() + ' (' + (body && body.reason || 'auto') + ')');
  const up = await runPool('upsert', {
    profile: profile, label: label, note: note,
    cookie: cookie, source: 'browser',
  });
  if (up && up.error) { syncLog('upsert 失败: ' + up.error); return { error: '入池失败: ' + up.error }; }

  // 绑定出口：让 API 侧复用浏览器同一个代理，避免直连/换 IP 把会话判死
  let boundProxyId = 0;
  if (up && up.id) {
    boundProxyId = await resolveProxyId();
    if (boundProxyId > 0) {
      const b = await runPool('set_proxy', { id: up.id, proxy_id: boundProxyId });
      if (b && b.error) { boundProxyId = 0; syncLog('绑定出口失败 #' + up.id + ': ' + b.error); }
    }
  }

  // 只在会话真正变化时才打上游校验（见 sessionFingerprint 说明）
  let check = null;
  let checkReused = false;
  try {
    const fp = sessionFingerprint(cookie);
    const cached = _checkCache.get(profile);
    const sameSession = cached && cached.fp === fp && cached.check && cached.check.ok
      && (Date.now() - (cached.at || 0)) < CHECK_CACHE_TTL_MS;
    if (sameSession) {
      check = cached.check;
      checkReused = true;
    } else {
      const jar = await adminLogin();
      if (jar && up && up.id) {
        check = await apiCheck(up.id, jar);
        if (check && check.ok) _checkCache.set(profile, { fp: fp, check: check, at: Date.now() });
        else _checkCache.delete(profile);
      }
    }
  } catch (e) { syncLog('check 异常: ' + e.message); }

  // 校验明确说「会话无效」（重定向登录页 / 页面无 SNlM0e）时，标记该账号需要重登。
  // 这类失败不是抓取时机的问题，重复抓只会拿到同一份废 cookie —— 写进 state 让
  // 看门狗/重登脚本接手，比一遍遍重抓有意义。
  let needsRelogin = false;
  if (check && !check.ok) {
    const d = String(check.detail || '');
    if (/重定向到登录页|no SNlM0e|没有登录态/.test(d)) {
      needsRelogin = true;
      syncLog('⚠ profile=' + profile + ' 会话无效（' + d + '）→ 标记需要重新登录');
      try {
        require('fs').writeFileSync('/var/lib/gw2a-cookie-sync/needs-relogin-' + profile + '.json',
          JSON.stringify({ at: Date.now(), profile: profile, detail: d, account_id: up.id }, null, 1));
      } catch (e) {}
    }
  }

  const removed = (up && up.removed) || 0;
  const detail = (up.action === 'inserted' ? '新增入池' : '刷新入池') + ' #' + up.id +
    ' profile=' + profile + ' cookie=' + up.cookie_len + 'B' +
    (boundProxyId > 0 ? ' proxy=#' + boundProxyId : ' proxy=未绑定') +
    (removed ? ' 清理旧记录=' + removed : '') +
    (check ? (check.ok ? ' ✓可用' + (checkReused ? '(同会话,跳过复检)' : '')
                       : ' ✗' + (check.detail || '不可用')) : ' (未校验)');
  syncLog(detail);
  return { ok: true, id: up.id, action: up.action, detail: detail,
           check: check, cookie_len: up.cookie_len, ts: up.ts,
           proxy_id: boundProxyId, removed: removed, needs_relogin: needsRelogin };
}

const HELPER_PAGE = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="UTF-8">
<title>Gemini Cookie Sync - 扩展桥接页</title>
<style>
body{font:14px/1.6 -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'PingFang SC','Microsoft YaHei',sans-serif;
max-width:720px;margin:40px auto;padding:0 16px;color:#1f1f1f;background:#f8fafd}
h1{font-size:18px}code{background:#eef1f6;padding:2px 6px;border-radius:4px}
pre{background:#0f1115;color:#e8eaed;padding:12px;border-radius:10px;overflow:auto;min-height:80px}
button{background:#0b57d0;color:#fff;border:0;border-radius:8px;padding:8px 14px;margin:4px 6px 4px 0;cursor:pointer}
.muted{color:#5f6368}
</style></head><body>
<h1>Gemini Cookie Sync 扩展桥接页</h1>
<p>本页由宿主机控制器托管，用于远程驱动已安装的扩展（保活 / 立即入池）。</p>
<p class="muted">扩展 ID：<code id="eid">__EXT_ID__</code></p>
<div>
<button onclick="call('ping')">Ping 扩展</button>
<button onclick="call('status')">状态</button>
<button onclick="call('keepalive')">立即保活</button>
<button onclick="call('sync')">立即入池</button>
</div>
<pre id="out">点击按钮测试扩展是否就绪…</pre>
<script>
var EXT_ID = '__EXT_ID__';
function call(type) {
  var out = document.getElementById('out');
  out.textContent = '请求中… ' + type;
  try {
    chrome.runtime.sendMessage(EXT_ID, { type: type }, function (resp) {
      var err = chrome.runtime.lastError;
      out.textContent = err ? ('扩展未响应: ' + err.message) : JSON.stringify(resp, null, 2);
    });
  } catch (e) { out.textContent = '调用失败: ' + e.message; }
}
</script></body></html>
`;

const MAX_PROFILES = 12;

// ── 按需唤醒 / 空闲休眠（2026-09-18 用户要求）────────────────────────────
// 「只有抓 cookie 时才唤醒浏览器，抓完就睡」：Chromium 常驻时 12 个进程
// ~1.9GB 内存 + ~30% CPU（其中 renderer 16% + gpu-process 11% 都是空转），
// 而真正需要它的只有每次抓取那几十秒。
//
// 生命周期：POST /profiles（抓取前的唤醒）→ 抓取（CDP 流量）→ 空闲 IDLE_STOP_SEC
// 无任何 CDP 流量 → 优雅 SIGTERM 收工（cookie 正常落盘）。用户手动登录期间
// 由 POST /profiles/<name>/hold 挂住，不自动睡。
//
// IDLE_STOP_SEC=0 关闭自动休眠（回到常驻行为）。
const IDLE_STOP_SEC = (() => {
  const v = parseInt(process.env.GW2A_IDLE_STOP_SEC || '180', 10);
  return Number.isFinite(v) && v >= 0 ? v : 180;
})();

const state = {
  profiles: new Map(), // name -> { name, port, pid, userDataDir, startedAt, lastUsedAt, hold }
};

function touchProfile(rec) {
  if (rec) rec.lastUsedAt = Date.now();
}

// sweepIdle 把空闲超时的 profile 优雅停掉。每 30s 跑一次。
function sweepIdle() {
  if (!IDLE_STOP_SEC) return;
  const now = Date.now();
  for (const [name, rec] of state.profiles) {
    if (rec.hold) continue;
    const idle = now - (rec.lastUsedAt || rec.startedAt || now);
    if (idle < IDLE_STOP_SEC * 1000) continue;
    console.log('[gw2a-ctrl] idle', Math.round(idle / 1000) + 's, stopping profile', name);
    try { stopProfile(name, { reason: 'idle' }); } catch (e) {
      console.log('[gw2a-ctrl] idle stop failed', name, e.message);
    }
  }
}
setInterval(sweepIdle, 30000).unref();

function safeName(name) {
  const s = String(name || '').trim().replace(/[^A-Za-z0-9._-]/g, '_').slice(0, 40);
  return s;
}

function sh(cmd, timeout) {
  return execSync(cmd, { timeout: timeout || 5000, encoding: 'utf8', shell: '/bin/sh' });
}

function portInUse(port) {
  try {
    const out = sh(`netstat -tln 2>/dev/null | grep -E '[:.]${port} ' || true`);
    return /\S/.test(out);
  } catch (e) { return false; }
}

function nextPort() {
  const used = new Set();
  for (const rec of state.profiles.values()) used.add(rec.port);
  for (let i = 0; i < MAX_PROFILES * 4; i++) {
    const p = CDP_BASE + i;
    if (used.has(p)) continue;
    if (portInUse(p)) continue;
    return p;
  }
  throw new Error('no free CDP port');
}

function profilePath(name) {
  return path.join(PROFILE_BASE, safeName(name));
}

function pidAlive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return false; }
}

let _creds = null;
function runUserCreds() {
  if (_creds) return _creds;
  try {
    const uid = parseInt(sh('id -u ' + RUN_USER).trim(), 10);
    const gid = parseInt(sh('id -g ' + RUN_USER).trim(), 10);
    _creds = { uid: uid, gid: gid };
  } catch (e) {
    _creds = { uid: undefined, gid: undefined };
  }
  return _creds;
}


// 启动时收养仍在跑的 profile chromium（控制器重启不丢登录窗口）
//
// 收养来的实例**照常参与空闲休眠**（hold=false，计时从现在起算）——
// 「不抓取就休眠」是稳态要求，控制器重启不该让浏览器永久常驻。
// 万一用户正在 VNC 里手动登录时控制器崩了：重新点一次「容器内打开」即可
// 再次挂住（那一步会显式 hold=true）。
function adoptRunning() {
  try {
    const out = sh(`ps -eo pid=,args= | grep -F -- '--user-data-dir=${PROFILE_BASE}/' | grep -v grep || true`, 8000);
    for (const line of out.split('\n')) {
      const m = line.match(/^\s*(\d+)\s+(.*)$/);
      if (!m) continue;
      const pid = parseInt(m[1], 10);
      const args = m[2];
      if (args.indexOf('--type=') >= 0) continue; // 只认主进程
      const portM = args.match(/--remote-debugging-port=(\d+)/);
      const dirM = args.match(new RegExp('--user-data-dir=(' + PROFILE_BASE.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '/[^ ]+)'));
      if (!portM || !dirM) continue;
      const name = path.basename(dirM[1]);
      if (!name || state.profiles.has(name)) continue;
      state.profiles.set(name, {
        name, port: parseInt(portM[1], 10), pid,
        userDataDir: dirM[1], startedAt: Date.now(),
        lastUsedAt: Date.now(), hold: false,
      });
    }
    if (state.profiles.size) console.log('[gw2a-ctrl] adopted', state.profiles.size, 'running profile(s)');
  } catch (e) { /* ignore */ }
}

function launchProfile(name, opts) {
  const sname = safeName(name);
  if (!sname) return { error: 'profile 名非法' };
  const existing = state.profiles.get(sname);
  if (existing && pidAlive(existing.pid)) return existing;

  const dir = profilePath(sname);
  try { fs.mkdirSync(dir, { recursive: true }); } catch (e) {}
  try { sh(`chown -R ${RUN_USER}:${RUN_USER} ${JSON.stringify(dir)}`, 20000); } catch (e) {}
  for (const f of ['SingletonLock', 'SingletonCookie', 'SingletonSocket']) {
    try { fs.unlinkSync(path.join(dir, f)); } catch (e) {}
  }

  const port = nextPort();
  // Load the bundled Gemini Cookie Sync extension (skipped if absent).
  const extArgs = [];
  try {
    if (fs.existsSync(path.join(EXT_DIR, 'manifest.json'))) {
      extArgs.push('--load-extension=' + EXT_DIR,
                   '--disable-extensions-except=' + EXT_DIR);
    }
  } catch (e) {}
  const args = [
    ...CHROME_FLAGS,
    ...extArgs,
    '--remote-debugging-address=127.0.0.1',
    '--remote-debugging-port=' + port,
    '--user-data-dir=' + dir,
    '--window-size=1200,700',
    '--window-position=0,0',
    // 单页面启动：抓取脚本只会复用这一个页面，不会开新窗口。
    // 启动页直接是 Gemini（见 START_URL 注释），唤醒即可只读抓取。
    START_URL,
  ];
  const env = {
    PATH: RT + '/usr/bin:' + RT + '/usr/sbin:' + RT + '/bin:/usr/bin:/bin',
    HOME: BROWSER_HOME,
    DISPLAY: (opts && opts.display) || GPU_DISPLAY || DISPLAY,
    XDG_RUNTIME_DIR: BROWSER_RUN,
    LD_LIBRARY_PATH: LD_LIB,
    LANG: 'en_US.UTF-8',
  };
  const creds = runUserCreds();
  const child = spawn(CHROMIUM, args, {
    detached: true,
    stdio: ['ignore', 'ignore', 'ignore'],
    env: env,
    uid: creds.uid,
    gid: creds.gid,
  });
  child.unref();
  const rec = {
    name: sname, port, pid: child.pid, userDataDir: dir,
    startedAt: Date.now(), lastUsedAt: Date.now(),
    hold: !!(opts && opts.hold), // hold=1 时用户要手动登录，空闲也不睡
  };
  state.profiles.set(sname, rec);
  console.log('[gw2a-ctrl] launch', sname, 'pid', child.pid, 'port', port,
      'ext', (extArgs.length ? EXT_DIR : '(none)'),
      'hold', rec.hold ? 1 : 0);
  return rec;
}

// 只杀这一个 profile 的进程组：pkill 的 pattern 太宽会连带其它 profile 一起杀掉。
function killTree(pid, graceMs) {
  // 2026-09-12: was SIGKILL-only. Chromium flushes cookies to the SQLite store
  // on a clean shutdown; SIGKILL loses freshly rotated __Secure-1PSIDTS/SIDCC
  // values and the next start reads a stale session that Google then treats as
  // anonymous (no SNlM0e on /app). SIGTERM first, SIGKILL only as last resort.
  if (!pid || pid <= 1) return;
  const grace = (typeof graceMs === 'number' && graceMs >= 0) ? graceMs : 8000;
  try { process.kill(-pid, 'SIGTERM'); } catch (e) {
    try { process.kill(pid, 'SIGTERM'); } catch (e2) { return; }
  }
  const deadline = Date.now() + grace;
  while (Date.now() < deadline) {
    try { process.kill(pid, 0); } catch (e) { return; }
    try { execSync('sleep 0.25', { timeout: 2000 }); } catch (e) { break; }
  }
  try { process.kill(-pid, 'SIGKILL'); } catch (e) {
    try { process.kill(pid, 'SIGKILL'); } catch (e2) {}
  }
}

function stopProfile(name, opts) {
  const sname = safeName(name);
  const rec = state.profiles.get(sname);
  if (!rec) return { error: 'profile 不存在或未在运行', running: false };
  const dir = rec.userDataDir;
  // 2026-09-12: graceful first (see killTree). SIGKILL here silently discarded
  // the rotated session cookies and made the stored cookie look "signed out".
  try {
    sh(`pkill -TERM -f -- ${JSON.stringify('--user-data-dir=' + dir)} || true`, 8000);
  } catch (e) {}
  killTree(rec.pid, 10000);
  // Give Chromium a moment to flush Cookies, then sweep any leftover child.
  try { sh(`sleep 1; pkill -KILL -f -- ${JSON.stringify('--user-data-dir=' + dir)} || true`, 8000); } catch (e) {}
  state.profiles.delete(sname);
  console.log('[gw2a-ctrl] stop', sname, 'reason', (opts && opts.reason) || 'manual');
  return { ok: true, port: rec.port };
}

function listProfiles() {
  const items = [];
  for (const [name, rec] of state.profiles) {
    items.push({
      name, port: rec.port, pid: rec.pid,
      user_data_dir: rec.userDataDir, started_at: rec.startedAt,
      last_used_at: rec.lastUsedAt || 0, hold: !!rec.hold,
      idle_sec: Math.round((Date.now() - (rec.lastUsedAt || rec.startedAt || Date.now())) / 1000),
    });
  }
  return items;
}

function readJSON(req) {
  return new Promise((resolve) => {
    let body = '';
    req.on('data', (c) => { body += c; });
    req.on('end', () => {
      try { resolve(body ? JSON.parse(body) : {}); } catch (e) { resolve({}); }
    });
  });
}

function corsHeaders() {
  return {
    'Access-Control-Allow-Origin': '*',
    'Access-Control-Allow-Methods': 'GET, PUT, POST, DELETE, OPTIONS',
    'Access-Control-Allow-Headers': 'Content-Type, Authorization',
    'Cache-Control': 'no-store',
  };
}

function send(res, code, obj) {
  res.writeHead(code, Object.assign({ 'Content-Type': 'application/json; charset=utf-8' }, corsHeaders()));
  res.end(JSON.stringify(obj));
}

function knownPort(port) {
  port = parseInt(port, 10);
  if (!Number.isInteger(port) || port <= 0) return false;
  for (const rec of state.profiles.values()) if (rec.port === port) return true;
  return portInUse(port);
}

// recByPort 找到该 CDP 端口对应的 profile 记录（用于刷新 lastUsedAt）。
function recByPort(port) {
  port = parseInt(port, 10);
  for (const rec of state.profiles.values()) if (rec.port === port) return rec;
  return null;
}

// HTTP 反向代理：/cdp/<port><restWithQuery> → http://127.0.0.1:<port><restWithQuery>
//
// ★ 每次 CDP 流量都刷新 lastUsedAt ★ —— 这就是「抓取中」的信号：
// 抓取脚本（refresh.py / gemini-web2api 容器）全程走这个隧道，抓完不再有流量，
// 空闲计时器到点就把 Chromium 收掉。
function cdpHTTPProxy(req, res, port, restWithQuery) {
  touchProfile(recByPort(port));
  const upReq = http.request({
    host: '127.0.0.1',
    port: port,
    path: restWithQuery,
    method: req.method || 'GET',
    headers: Object.assign({}, req.headers, { host: '127.0.0.1:' + port }),
  }, (upRes) => {
    res.writeHead(upRes.statusCode || 502, Object.assign({}, upRes.headers, corsHeaders()));
    upRes.pipe(res);
  });
  upReq.on('error', (e) => {
    if (!res.headersSent) send(res, 502, { error: 'cdp proxy error: ' + e.message });
    else { try { res.destroy(); } catch (e2) {} }
  });
  req.pipe(upReq);
}

// WebSocket 反向隧道
function cdpWSTunnel(req, socket, head, port, pathname) {
  touchProfile(recByPort(port));
  const upReq = http.request({
    host: '127.0.0.1',
    port: port,
    path: pathname,
    method: 'GET',
    headers: Object.assign({}, req.headers, { host: '127.0.0.1:' + port }),
  });
  upReq.on('upgrade', (upRes, upSocket, upHead) => {
    let h = 'HTTP/1.1 ' + (upRes.statusCode || 101) + ' ' + (upRes.statusMessage || 'Switching Protocols') + '\r\n';
    for (const k of Object.keys(upRes.headers)) {
      const v = upRes.headers[k];
      if (Array.isArray(v)) v.forEach((x) => { h += k + ': ' + x + '\r\n'; });
      else h += k + ': ' + v + '\r\n';
    }
    h += '\r\n';
    try { socket.write(h); } catch (e) {}
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
}

const server = http.createServer(async (req, res) => {
  if (req.method === 'OPTIONS') return send(res, 204, {});
  const url = req.url || '/';
  const p = url.split('?')[0];

  const cdpM = url.match(/^\/cdp\/(\d+)(\/[^?]*)(\?.*)?$/);
  if (cdpM) {
    const port = parseInt(cdpM[1], 10);
    if (!knownPort(port)) return send(res, 404, { error: 'unknown cdp port ' + port });
    return cdpHTTPProxy(req, res, port, cdpM[2] + (cdpM[3] || ''));
  }

  try {
    if (p === '/healthz' && req.method === 'GET') {
      return send(res, 200, { ok: true, profiles: listProfiles().length, port: PORT,
        cookie_sync: true, api: GW2A_API, pool_py: POOL_PY, ext_id: EXT_ID,
        ext_dir: EXT_DIR,
        idle_stop_sec: IDLE_STOP_SEC,
        ext_present: fs.existsSync(path.join(EXT_DIR, 'manifest.json')) });
    }
    if (p === '/profiles' && req.method === 'GET') {
      return send(res, 200, { profiles: listProfiles() });
    }
    if (p === '/cookie-sync' && req.method === 'POST') {
      const body = await readJSON(req);
      const tok = body._token || req.headers['x-gw2a-token'] || '';
      const sock = req.socket || {};
      const ip = sock.remoteAddress || (sock.socket && sock.socket.remoteAddress) || '';
      const out = await handleCookieSync(Object.assign({}, body, { _token: tok }), ip);
      if (out && out.error) return send(res, out.code || 400, out);
      return send(res, 200, out);
    }
    if (p === '/cookie-pool' && req.method === 'GET') {
      const list = await runPool('list', {});
      return send(res, 200, list);
    }
    if (p === '/extension' && req.method === 'GET') {
      res.writeHead(200, Object.assign({ 'Content-Type': 'text/html; charset=utf-8' }, corsHeaders()));
      return res.end(HELPER_PAGE.replace(/__EXT_ID__/g, EXT_ID));
    }
    if (p === '/extension-id' && req.method === 'GET') {
      return send(res, 200, { ext_id: EXT_ID, helper_page: 'http://127.0.0.1:' + PORT + '/extension' });
    }
    if (p === '/profiles' && req.method === 'POST') {
      const body = await readJSON(req);
      const name = body.name;
      if (!name) return send(res, 400, { error: '缺少 name' });
      if (state.profiles.size >= MAX_PROFILES) return send(res, 400, { error: 'profile 数量超限' });
      const existing = state.profiles.get(safeName(name));
      const launched = !(existing && pidAlive(existing.pid));
      const rec = launchProfile(name, { display: body.display, hold: body.hold });
      if (rec.error) return send(res, 400, rec);
      touchProfile(rec);
      // launched 告诉调用方「这次是真唤醒」——它需要多等几秒 CDP 就绪；
      // 已常驻的实例则立即可用（复用现有窗口，不新建页面）。
      return send(res, 200, Object.assign({}, rec, {
        launched, idle_stop_sec: IDLE_STOP_SEC,
      }));
    }
    const delM = p.match(/^\/profiles\/([^/]+)$/);
    if (delM && req.method === 'DELETE') {
      return send(res, 200, stopProfile(decodeURIComponent(delM[1]), { reason: 'delete' }));
    }
    const stopM = p.match(/^\/profiles\/([^/]+)\/stop$/);
    if (stopM && req.method === 'POST') {
      return send(res, 200, stopProfile(decodeURIComponent(stopM[1]), { reason: 'manual' }));
    }
    // hold：用户要在 VNC 里手动登录 → 挂住不自动睡；解除后照常空闲休眠。
    const holdM = p.match(/^\/profiles\/([^/]+)\/hold$/);
    if (holdM && req.method === 'POST') {
      const name = safeName(decodeURIComponent(holdM[1]));
      const body = await readJSON(req);
      const on = !(body && body.hold === false);
      const rec = state.profiles.get(name);
      if (!rec) return send(res, 404, { error: 'profile 不存在或未在运行' });
      rec.hold = on;
      touchProfile(rec);
      console.log('[gw2a-ctrl] hold', name, on ? 'on' : 'off');
      return send(res, 200, { ok: true, name, hold: on });
    }
    // 一次性抓取：唤醒 → 等 CDP 就绪 → 交给调用方。抓完不用管，
    // 空闲 IDLE_STOP_SEC 后自动睡。
    if (p === '/profiles/wake' && req.method === 'POST') {
      const body = await readJSON(req);
      const name = body.name;
      if (!name) return send(res, 400, { error: '缺少 name' });
      let rec = state.profiles.get(safeName(name));
      const launched = !(rec && pidAlive(rec.pid));
      if (launched) {
        if (state.profiles.size >= MAX_PROFILES) return send(res, 400, { error: 'profile 数量超限' });
        rec = launchProfile(name, { display: body.display, hold: body.hold });
        if (rec.error) return send(res, 400, rec);
      }
      touchProfile(rec);
      return send(res, 200, Object.assign({}, rec, {
        launched, idle_stop_sec: IDLE_STOP_SEC,
      }));
    }
    return send(res, 404, { error: 'not found', path: p });
  } catch (e) {
    return send(res, 500, { error: e.message });
  }
});

server.on('upgrade', (req, socket, head) => {
  const u = req.url || '';
  const m = u.match(/^\/cdp\/(\d+)(\/.*)$/);
  if (!m) { try { socket.destroy(); } catch (e) {} return; }
  const port = parseInt(m[1], 10);
  if (!knownPort(port)) {
    try { socket.end('HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n'); } catch (e) {}
    return;
  }
  cdpWSTunnel(req, socket, head, port, m[2]);
});

server.listen(PORT, '0.0.0.0', () => {
  console.log('[gw2a-ctrl] listening on 0.0.0.0:' + PORT + ', profiles=' + PROFILE_BASE + ', cdp base=' + CDP_BASE + ', display=' + DISPLAY);
  console.log('[gw2a-ctrl] cookie-sync -> ' + POOL_PY + ' (api ' + GW2A_API + ', ext ' + (EXT_ID || 'unset') + ')');
});

adoptRunning();

process.on('SIGTERM', () => { process.exit(0); });
process.on('SIGINT', () => { process.exit(0); });
