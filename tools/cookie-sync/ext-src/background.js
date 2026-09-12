/**
 * Gemini Cookie Sync — background service worker (MV3)
 * ---------------------------------------------------
 * 职责（在服务器自带的 Chromium 里运行，随 profile 常驻）：
 *   1) 保活：每 10 分钟刷新一次已打开的 gemini.google.com 页面，让 Google 正常
 *      轮换 __Secure-1PSIDTS / SIDCC 等会话 cookie，避免会话静默失效。
 *   2) 取 cookie：用 chrome.cookies API 读取当前 profile 里真实、已轮换过的
 *      Gemini Web 会话 cookie（SID/HSID/SSID/APISID/SAPISID/__Secure-*）。
 *   3) 入池：每 30 分钟（以及登录成功/手动触发时）把 cookie 推送到宿主机上的
 *      gemini-web2api 控制器 /cookie-sync，由控制器写入容器内 SQLite cookie 池。
 *
 * ★ 风控规避（v1.0.5 起）★
 *   不再「每次抓取都新开一个页面」——那样短时间内在同一个 Google 账号下反复
 *   产生新会话，极易触发风控（会话被踢、出现 sorry 页）。现在改为：
 *     · 复用同一个 Gemini 标签页，用 chrome.tabs.reload() 的方式刷新，
 *       让服务端重新下发轮换 cookie；
 *     · 每次刷新后进入 refreshCooldownSec（默认 60 秒）冷却窗口，
 *       窗口内只做 cookie 提取，绝不做任何导航；
 *     · 只有冷却结束、且仍未拿到可用 cookie 时，才允许再刷新一次。
 *
 * 配置存在 chrome.storage.local：
 *   controller         控制器地址，默认 http://127.0.0.1:9280
 *   profile            本 profile 在池中的标识，默认 browser1
 *   token              控制器共享密钥（可空）
 *   refreshCooldownSec 刷新冷却窗口（秒），默认 60
 *   autoKeepalive / keepalivePeriodMin / autoSyncMin / enabled
 */

'use strict';

const DEFAULTS = {
  controller: 'http://127.0.0.1:9280',
  profile: '',
  token: '',
  enabled: true,
  autoKeepalive: true,
  keepalivePeriodMin: 10,
  autoSyncMin: 30,
  refreshCooldownSec: 60,
  lastSyncAt: 0,
  lastSyncOk: false,
  lastSyncDetail: '',
  lastKeepaliveAt: 0,
  lastKeepaliveDetail: '',
  lastRotateAt: 0,
  lastRefreshAt: 0,
  lastRefreshDetail: '',
  lastTabId: 0,          // 复用的 Gemini 标签页（含被重定向到 sorry 页的情况）
  lastTabUrl: '',
};

const ALARM_KEEPALIVE = 'gw2a-keepalive';
const ALARM_SYNC = 'gw2a-sync';
const GEMINI_URL = 'https://gemini.google.com/app';
const GEMINI_URL_RE = /^https:\/\/gemini\.google\.com\//;

// 被 Google 风控拦下时的落地页（www.google.com/sorry/...）。这种页面同样是
// 「本 profile 的那个 Gemini 标签页」，必须复用而不是另开新页。
const SORRY_URL_RE = /^https:\/\/(www\.)?google\.com\/sorry\//;

// 池里真正需要的 cookie（顺序即写入池的顺序，SAPISID 放最前便于人工核对）
const COOKIE_ORDER = [
  'SAPISID', '__Secure-1PAPISID', '__Secure-3PAPISID', 'APISID',
  'SID', 'HSID', 'SSID', '__Secure-1PSID', '__Secure-3PSID',
  '__Secure-1PSIDTS', '__Secure-3PSIDTS', '__Secure-1PSIDCC', '__Secure-3PSIDCC',
  'SIDCC', 'LSID', '__Host-1PLSID', '__Host-3PLSID',
  'ACCOUNT_CHOOSER', 'NID', 'COMPASS',
];
// 登录判定用「任一组命中」而不是「全部命中」：
// 不同 Google 站点下发的 cookie 组合不完全一致（例如只拿到 SAPISID+__Secure-1PSID
// 而没有 SID），硬性要求全部存在会把已登录会话误判成未登录。
const AUTH_ANY = [
  ['SAPISID', 'SID', '__Secure-1PSID'],   // 组 1：任一存在
  ['__Secure-1PSID', '__Secure-3PSID'],   // 组 2：任一存在
];

// 同一时刻只允许一个 sync/keepalive 在跑，避免「刷新 → onUpdated → sync」
// 这类回调互相触发、连环刷新页面。
let BUSY = false;
let BUSY_SINCE = 0;
const BUSY_MAX_MS = 150000;   // 锁最长持有时间；超过则视为悬挂，自动释放

/** 拿锁；若上一轮已悬挂超过 BUSY_MAX_MS 则自动释放并接管。 */
function acquireBusy() {
  if (BUSY && Date.now() - BUSY_SINCE < BUSY_MAX_MS) return false;
  if (BUSY) log('检测到悬挂的任务锁（已持有 ' +
    Math.round((Date.now() - BUSY_SINCE) / 1000) + 's），强制接管');
  BUSY = true;
  BUSY_SINCE = Date.now();
  return true;
}

function releaseBusy() { BUSY = false; BUSY_SINCE = 0; }

// ---------------------------------------------------------------- helpers

async function cfg() {
  const got = await chrome.storage.local.get(DEFAULTS);
  return Object.assign({}, DEFAULTS, got);
}

async function setCfg(patch) {
  await chrome.storage.local.set(patch);
  return cfg();
}

function log(...a) {
  console.log('[gw2a-ext]', ...a);
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

function cooldownMs(c) {
  const s = Number((c && c.refreshCooldownSec) || 0) || 60;
  return Math.max(10, Math.min(3600, s)) * 1000;
}

function sha1Hex(str) {
  const buf = new TextEncoder().encode(str);
  return crypto.subtle.digest('SHA-1', buf).then((h) => {
    return Array.from(new Uint8Array(h)).map((b) => b.toString(16).padStart(2, '0')).join('');
  });
}

/** 当前 profile 的标识：优先用配置，其次用默认名。 */
async function profileName() {
  const c = await cfg();
  return c.profile || 'browser1';
}

// ---------------------------------------------------------------- cookies

/** 站点列表：逐项 get() 时依次尝试，能命中登录 cookie 就算成功。 */
const READ_URLS = [
  'https://gemini.google.com/',
  'https://accounts.google.com/',
  'https://www.google.com/',
  'https://google.com/',
];

/**
 * 读取 google 域下全部 cookie，返回 name -> cookie 对象。
 *
 * 为什么不能只用 getAll()：Chromium 152 的 chrome.cookies.getAll({domain})
 * 会漏掉 SID / HSID / APISID 这类老式登录 cookie（实测只返回 19 条，
 * 缺的正好是判定登录所必需的那几个），于是 isLoggedIn() 永远为 false。
 * 但 chrome.cookies.get({url, name}) 能逐项取到它们（实测 SID len=153）。
 * 所以：getAll 拿全量打底，再用 get() 把 COOKIE_ORDER 里缺的逐个补齐。
 */
async function readGoogleCookies() {
  const seen = new Map();
  const score = (x) => (x.domain === '.google.com' ? 2 : 0) + (x.httpOnly ? 2 : 0) + (x.secure ? 1 : 0);
  const put = (ck) => {
    if (!ck || !ck.name) return;
    const prev = seen.get(ck.name);
    if (!prev) { seen.set(ck.name, ck); return; }
    if (!prev.value && ck.value) { seen.set(ck.name, ck); return; }
    if (score(ck) > score(prev)) seen.set(ck.name, ck);
  };
  const push = (list) => { for (const ck of list) put(ck); };

  try { push(await chrome.cookies.getAll({ domain: '.google.com' })); } catch (e) { log('getAll google.com failed', e); }
  for (const u of READ_URLS) {
    try { push(await chrome.cookies.getAll({ url: u })); } catch (e) {}
  }
  try { push(await chrome.cookies.getAll({})); } catch (e) {}

  // 补齐：getAll 在部分 Chromium 版本会漏项，逐项 get() 兜底
  const missing = COOKIE_ORDER.filter((n) => {
    const ck = seen.get(n);
    return !ck || !ck.value;
  });
  if (missing.length) {
    for (const name of missing) {
      for (const u of READ_URLS) {
        try {
          const ck = await chrome.cookies.get({ url: u, name });
          if (ck && ck.value) { put(ck); break; }
        } catch (e) {}
      }
    }
    const still = missing.filter((n) => !(seen.get(n) || {}).value);
    if (still.length) log('get() 补齐后仍缺:', still.join(','));
  }
  return seen;
}

/** 组装成 Cookie 头字符串 + 结构体。 */
function buildCookieHeader(map) {
  const parts = [];
  const obj = {};
  for (const name of COOKIE_ORDER) {
    const ck = map.get(name);
    if (ck && ck.value) { parts.push(name + '=' + ck.value); obj[name] = ck.value; }
  }
  // 兜底：把剩下未列出的 google 会话类 cookie 也带上（跳过统计类）
  for (const [name, ck] of map) {
    if (obj[name] || !ck.value) continue;
    if (/^(_ga|_gcl|_gid|OTZ|COMPASS)/.test(name)) continue;
    parts.push(name + '=' + ck.value);
    obj[name] = ck.value;
  }
  return { header: parts.join('; '), obj };
}

function cookieSummary(map) {
  const now = Date.now() / 1000;
  const out = {};
  for (const name of COOKIE_ORDER) {
    const ck = map.get(name);
    if (!ck) continue;
    out[name] = {
      len: (ck.value || '').length,
      expires: ck.expirationDate || null,
      expired: !!(ck.expirationDate && ck.expirationDate < now),
    };
  }
  return out;
}

function isLoggedIn(map) {
  const now = Date.now() / 1000;
  const alive = (k) => {
    const ck = map.get(k);
    if (!ck || !ck.value) return false;
    if (ck.expirationDate && ck.expirationDate < now) return false;
    return true;
  };
  for (const group of AUTH_ANY) {
    if (group.some(alive)) return true;
  }
  return false;
}

// ---------------------------------------------------------------- tabs

/** 找一个 gemini 标签页；没有则返回 null（不创建）。 */
async function findGeminiTab() {
  let tabs = [];
  try {
    tabs = await chrome.tabs.query({ url: ['https://gemini.google.com/*'] });
  } catch (e) {
    return null;
  }
  if (!tabs.length) return null;
  tabs.sort((a, b) => (b.lastAccessed || 0) - (a.lastAccessed || 0));
  return tabs[0];
}
/**
 * 找到「本 profile 的那个 Gemini 标签页」，没有则返回 null（不创建）。

 * 关键点：chrome.tabs.query({url:'https://gemini.google.com/*'}) 匹配不到被
 * Google 风控重定向后的 www.google.com/sorry 页，旧代码因此会误判为「没有
 * Gemini 页」而 chrome.tabs.create() 再开一个 —— 这正是风控的来源。所以这里
 * 依次尝试：① 上次记录的 tabId；② URL 含 gemini.google.com；③ google.com/sorry 页。
 */
async function findGeminiTab() {
  const c = await cfg();
  // ① 上次记录的 tabId（最可靠：无论它现在停在哪个 URL 都认）
  const remembered = Number(c.lastTabId) || 0;
  if (remembered) {
    try {
      const t = await chrome.tabs.get(remembered);
      if (t && t.id != null) return t;
    } catch (e) { /* 已被关掉 */ }
  }
  let tabs = [];
  try {
    tabs = await chrome.tabs.query({ url: ['https://gemini.google.com/*'] });
  } catch (e) { tabs = []; }
  // ② 风控落地页（www.google.com/sorry/...）也要认，否则又会新建页面
  if (!tabs.length) {
    try {
      const all = await chrome.tabs.query({});
      tabs = all.filter((t) => SORRY_URL_RE.test(t.url || ''));
    } catch (e) { tabs = []; }
  }
  if (!tabs.length) return null;
  tabs.sort((a, b) => (b.lastAccessed || 0) - (a.lastAccessed || 0));
  return tabs[0];
}
/** 等某个标签页加载完成（或超时）。 */
function waitTabComplete(tabId, timeoutMs) {
  return new Promise((resolve) => {
    let done = false;
    const finish = () => { if (!done) { done = true; resolve(); } };
    const to = setTimeout(finish, timeoutMs || 30000);
    const onUpd = (id, info) => {
      if (id === tabId && info.status === 'complete') {
        chrome.tabs.onUpdated.removeListener(onUpd);
        clearTimeout(to);
        setTimeout(finish, 1500); // 留一点时间让 Set-Cookie 落盘
      }
    };
    chrome.tabs.onUpdated.addListener(onUpd);
  });
}

/**
 * 让 Gemini 页面「刷新」一次，而不是新开页面。
 *
 *   - 已有 gemini 标签页 → chrome.tabs.reload()（同一标签、同一会话）
 *   - 没有标签页（首次）→ 才创建 1 个（pinned），之后一直复用它
 *   - 距上次刷新不足 refreshCooldownSec 秒 → 直接返回 refreshed=false，
 *     调用方只能在冷却窗口里提取 cookie，不允许再导航
 */
async function refreshGeminiTab(force) {
  const c = await cfg();
  const cd = cooldownMs(c);
  const last = Number(c.lastRefreshAt) || 0;
  const since = Date.now() - last;
  if (!force && last && since < cd) {
    return { refreshed: false, cooldown_left_ms: cd - since, tab: await findGeminiTab() };
  }

  let tab = await findGeminiTab();
  let created = false;
  if (!tab) {
    try {
      tab = await chrome.tabs.create({ url: GEMINI_URL, active: false, pinned: true });
      created = true;
    } catch (e) {
      return { refreshed: false, error: '创建标签页失败: ' + e.message };
    }
  }

  // 先记时间戳：整个加载 + 冷却窗口内都不允许再次刷新
  await setCfg({ lastRefreshAt: Date.now() });

  try {
    if (!created) {
      if (GEMINI_URL_RE.test(tab.url || '')) await chrome.tabs.reload(tab.id);
      else await chrome.tabs.update(tab.id, { url: GEMINI_URL });
    }
  } catch (e) {
    await setCfg({ lastRefreshDetail: '刷新失败: ' + e.message });
    return { refreshed: false, error: '刷新失败: ' + e.message, tab };
  }

  await waitTabComplete(tab.id, 30000);
  // 记住这个标签页：下次直接复用它，绝不再新建
  let finalUrl = '';
  try { const t = await chrome.tabs.get(tab.id); finalUrl = (t && t.url) || ''; } catch (e) {}
  const sorry = SORRY_URL_RE.test(finalUrl);
  await setCfg({
    lastTabId: tab.id,
    lastTabUrl: finalUrl,
    lastRefreshDetail: (created ? '首次创建页面' : '刷新页面')
      + (sorry ? '（被风控重定向到 sorry 页，本轮只提取 cookie）' : '')
      + ' @ ' + new Date().toISOString(),
  });
  log('refresh gemini tab', tab.id, created ? '(首次创建)' : '(reload)',
      sorry ? '[sorry 页]' : '');
  return { refreshed: true, created, sorry, tab };
}

/**
 * 在冷却窗口内反复提取 cookie：刷新后 Google 需要几秒才把新的
 * __Secure-1PSIDTS 写回 cookie store，所以这里以 3 秒为间隔轮询，
 * 直到拿到完整登录 cookie 或窗口耗尽。整个过程中不做任何导航。
 */
async function collectWithinCooldown(maxMs) {
  const budget = Math.max(3000, maxMs || 60000);
  const deadline = Date.now() + budget;
  let map = await readGoogleCookies();
  if (isLoggedIn(map)) return { map, waited_ms: 0, exhausted: false };
  while (Date.now() < deadline) {
    await sleep(3000);
    map = await readGoogleCookies();   // 每次调用都会刷新 SW 的空闲计时
    if (isLoggedIn(map)) {
      return { map, waited_ms: budget - Math.max(0, deadline - Date.now()), exhausted: false };
    }
  }
  return { map, waited_ms: budget, exhausted: true };
}

/**
 * 保活：刷新 gemini 页面（受 60 秒冷却约束），并在页面里探活，
 * 让服务端下发新的 __Secure-1PSIDTS/SIDCC（浏览器会自动写回 cookie store）。
 */
async function keepalive(manual) {
  const c = await cfg();
  if (!c.enabled && !manual) return { ok: false, detail: '扩展已停用' };
  if (!acquireBusy()) return { ok: false, detail: '上一轮任务仍在进行' };
  try {
    // 注意：即便手动触发也不绕过 60 秒冷却，否则连续点按会连环刷新页面
    const r = await refreshGeminiTab(false);
    const tab = r.tab || (await findGeminiTab());
    if (!tab) return { ok: false, detail: r.error || '没有可用的 gemini 标签页' };

    let probe = null;
    if (r.refreshed) {
      try {
        const res = await chrome.scripting.executeScript({
          target: { tabId: tab.id },
          func: () => {
            const html = document.documentElement.innerHTML || '';
            const m = html.match(/SNlM0e\s*[:=]\s*"(.*?)"/) || html.match(/SNlM0e["']?\s*:\s*"([^"]+)"/);
            const txt = (document.body && document.body.innerText || '').slice(0, 600);
            return {
              url: location.href,
              title: document.title,
              snlm0e: !!m,
              signed_out: /Sign in to save activity|^Sign in$/m.test(txt),
            };
          },
        });
        probe = res && res[0] && res[0].result;
      } catch (e) {
        probe = { error: e.message };
      }
    }

    const map = await readGoogleCookies();
    const logged = isLoggedIn(map);
    const detail = (r.refreshed
      ? '已刷新页面'
      : '冷却中（剩余 ' + Math.ceil((r.cooldown_left_ms || 0) / 1000) + 's，不刷新）')
      + '；' + (logged ? '会话有效' : '未检测到完整登录 cookie');
    const out = { ok: true, refreshed: !!r.refreshed, logged_in: logged, probe, detail };
    await setCfg({ lastKeepaliveAt: Date.now(), lastKeepaliveDetail: detail });
    log('keepalive', out);
    if (logged) await sync('keepalive', true);   // 内部调用：复用外层 BUSY，避免自锁
    return out;
  } finally {
    releaseBusy();
  }
}

/**
 * 主动请求 Google 轮换 __Secure-1PSIDTS。
 *
 * Gemini 的会话靠 __Secure-1PSIDTS / SIDCC 这类「轮换 cookie」维持；服务端会在
 * 你每次正常访问时下发新值。这里额外显式请求一次，让「重新生成会话 cookie」
 * 不依赖页面刷新时机。失败无所谓（页面刷新同样会轮换），绝不因此中断入池。
 */
async function forceRotateCookies() {
  // 必须在 gemini 页面上下文里发：这样 Origin 是 https://gemini.google.com，
  // 且带上该站点的第一方 Cookie，Google 才会真的下发新的 __Secure-1PSIDTS。
  // 从 service worker 直接 fetch 会因 Origin 不对被拒（实测 400）。
  try {
    const tab = await findGeminiTab();
    if (!tab) return false;
    const res = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      world: 'MAIN',
      func: async () => {
        try {
          const r = await fetch('https://accounts.google.com/RotateCookies', {
            method: 'POST',
            credentials: 'include',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify([{ cookieName: '__Secure-1PSIDTS', refreshStrategy: 'ROTATE' }]),
          });
          return { ok: r.ok, status: r.status };
        } catch (e) { return { ok: false, error: String(e) }; }
      },
    });
    const out = res && res[0] && res[0].result;
    log('rotate cookies', out);
    return !!(out && out.ok);
  } catch (e) {
    log('rotate cookies failed (忽略)', e);
    return false;
  }
}

// ---------------------------------------------------------------- sync

/**
 * 把当前 cookie 推送到控制器（由控制器写入容器内 cookie 池）。
 *
 * 节奏（风控友好）：
 *   1) 距上次刷新 ≥ 60s → 刷新一次页面，然后在 60s 冷却窗口里轮询提取；
 *   2) 距上次刷新 < 60s → 完全不导航，直接提取当前 cookie；
 *   3) 提取不到登录 cookie 时，直接跳过入池（不反复刷新）。
 */
async function sync(reason, _internal) {
  const c = await cfg();
  if (!c.enabled && reason !== 'manual' && reason !== 'host') {
    return { ok: false, detail: '扩展已停用' };
  }
  const own = !_internal;                 // 内部调用（keepalive）不重复加锁
  if (own) {
    if (!acquireBusy()) return { ok: false, detail: '上一轮任务仍在进行' };
  }
  try {
    const cd = cooldownMs(c);
    const last = Number(c.lastRefreshAt) || 0;
    const since = Date.now() - last;
    let refreshed = false;
    let map = await readGoogleCookies();
    let logged = isLoggedIn(map);

    if (!logged || since >= cd) {
      // 冷却已过（或当前读不到登录 cookie）：允许刷新一次
      // 一律不 force：手动触发也受 60 秒冷却约束，防止连点造成连环刷新
      const r = await refreshGeminiTab(false);
      refreshed = !!r.refreshed;   // 冷却未过时 r.refreshed=false，只提取不导航
      if (refreshed) {
        const got = await collectWithinCooldown(cd);
        map = got.map;
        logged = isLoggedIn(map);
        if (got.exhausted && !logged) log('冷却窗口内仍未取到登录 cookie');
      } else {
        map = await readGoogleCookies();
        logged = isLoggedIn(map);
      }
    } else {
      log('冷却中，跳过刷新，直接提取 cookie（剩余 ' +
          Math.ceil((cd - since) / 1000) + 's）');
    }

    if (!map.size) return { ok: false, detail: '没有读到 google cookie' };
    if (!logged) {
      const r = { ok: false, detail: '未登录（缺少 SID/SAPISID/__Secure-1PSID），跳过入池' };
      await setCfg({ lastSyncAt: Date.now(), lastSyncOk: false, lastSyncDetail: r.detail });
      return r;
    }

    // 登录态 OK 时顺手触发一次轮换（60s 内只做一次），再重读
    if (Date.now() - (Number(c.lastRotateAt) || 0) > 60000) {
      await forceRotateCookies();
      await setCfg({ lastRotateAt: Date.now() });
      await sleep(800);                       // 等 Set-Cookie 落盘
      map = await readGoogleCookies();
    }

    const { header } = buildCookieHeader(map);
    const sapisid = (map.get('SAPISID') || {}).value || '';
    const ts = Math.floor(Date.now() / 1000);
    const origin = 'https://gemini.google.com';
    const sapisidhash = sapisid
      ? ts + '_' + (await sha1Hex(ts + ' ' + sapisid + ' ' + origin))
      : '';
    const profile = await profileName();

    const payload = {
      profile,
      reason: reason || 'auto',
      url: GEMINI_URL,
      ts,
      cookie: header,
      sapisidhash,
      refreshed,
      rotated: Date.now() - (Number(c.lastRotateAt) || 0) < 120000,
      logged_in: true,
      user_agent: navigator.userAgent,
      summary: cookieSummary(map),
      client: 'gw2a-ext/1.0.5',
    };

    const url = c.controller.replace(/\/+$/, '') + '/cookie-sync';
    let r;
    try {
      const resp = await fetch(url, {
        method: 'POST',
        headers: Object.assign({ 'Content-Type': 'application/json' },
          c.token ? { 'X-GW2A-Token': c.token } : {}),
        body: JSON.stringify(payload),
      });
      const text = await resp.text();
      let body = null;
      try { body = JSON.parse(text); } catch (e) { body = { raw: text }; }
      r = { ok: resp.ok && !(body && body.error), http: resp.status, body };
      if (!r.ok) r.detail = '控制器返回 ' + resp.status + ' ' + ((body && (body.error || body.detail)) || text.slice(0, 120));
      else r.detail = (body && body.detail) || '已入池';
    } catch (e) {
      r = { ok: false, detail: '推送失败: ' + e.message };
    }

    await setCfg({
      lastSyncAt: Date.now(),
      lastSyncOk: !!r.ok,
      lastSyncDetail: r.detail || '',
    });
    log('sync', r);
    return r;
  } finally {
    if (own) releaseBusy();
  }
}

// ---------------------------------------------------------------- alarms

async function ensureAlarms() {
  const c = await cfg();
  await chrome.alarms.clear(ALARM_KEEPALIVE);
  await chrome.alarms.clear(ALARM_SYNC);
  if (!c.enabled) return;
  if (c.autoKeepalive) {
    chrome.alarms.create(ALARM_KEEPALIVE, {
      delayInMinutes: 0.2,
      periodInMinutes: Math.max(1, Number(c.keepalivePeriodMin) || 10),
    });
  }
  chrome.alarms.create(ALARM_SYNC, {
    delayInMinutes: 1,
    periodInMinutes: Math.max(1, Number(c.autoSyncMin) || 30),
  });
}

chrome.alarms.onAlarm.addListener(async (a) => {
  try {
    if (a.name === ALARM_KEEPALIVE) {
      await keepalive(false);
    } else if (a.name === ALARM_SYNC) {
      const c = await cfg();
      // 先刷新 gemini 页面让 Google 轮换会话 cookie，keepalive 内部会紧接着 sync；
      // 保活关闭时退化为直接 sync。
      if (c.enabled && c.autoKeepalive) await keepalive(false);
      else await sync('alarm');
    }
  } catch (e) { log('alarm error', e); }
});

// ---------------------------------------------------------------- lifecycle

chrome.runtime.onInstalled.addListener(async (d) => {
  log('installed', d.reason);
  await ensureAlarms();
  try { await sync('installed'); } catch (e) {}
});

chrome.runtime.onStartup.addListener(async () => {
  await ensureAlarms();
  try { await keepalive(false); } catch (e) {}
});

// gemini 页面加载完成时顺手同步（登录后立刻入池）。
// 注意：sync 内部有 60 秒冷却保护，这里不会引起连环刷新。
chrome.tabs.onUpdated.addListener(async (tabId, info, tab) => {
  if (info.status !== 'complete') return;
  if (!tab || !GEMINI_URL_RE.test(tab.url || '')) return;
  if (BUSY) return;                       // 自己刷新引起的加载，直接忽略
  try {
    const c = await cfg();
    if (!c.enabled) return;
    const map = await readGoogleCookies();
    if (isLoggedIn(map)) await sync('page-load');
  } catch (e) { log('onUpdated sync error', e); }
});

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    try {
      switch (msg && msg.type) {
        case 'status': {
          const c = await cfg();
          const map = await readGoogleCookies();
          const last = Number(c.lastRefreshAt) || 0;
          const cd = cooldownMs(c);
          sendResponse({
            ok: true, config: c, logged_in: isLoggedIn(map),
            cookie_count: map.size, summary: cookieSummary(map),
            cooldown_left_ms: Math.max(0, cd - (Date.now() - last)),
            tabs: (await chrome.tabs.query({ url: ['https://gemini.google.com/*'] })).length,
          });
          break;
        }
        case 'setConfig':
          sendResponse({ ok: true, config: await setCfg(msg.patch || {}) });
          await ensureAlarms();
          break;
        case 'keepalive':
          sendResponse(await keepalive(true));
          break;
        case 'sync':
          sendResponse(await sync('manual'));
          break;
        default:
          sendResponse({ ok: false, detail: 'unknown message ' + (msg && msg.type) });
      }
    } catch (e) {
      sendResponse({ ok: false, detail: String((e && e.message) || e) });
    }
  })();
  return true; // async response
});

ensureAlarms().catch((e) => log('ensureAlarms failed', e));

// 宿主页（控制器托管的引导页）远程控制入口：
// chrome.runtime.sendMessage(EXT_ID, {...}) 即可触发保活/入池，无需用户点击。
chrome.runtime.onMessageExternal.addListener((msg, sender, sendResponse) => {
  (async () => {
    try {
      const cfgPatch = (msg && msg.patch) || null;
      if (cfgPatch) await setCfg(cfgPatch);
      switch (msg && msg.type) {
        case 'ping':
          sendResponse({ ok: true, pong: true, client: 'gw2a-ext/1.0.5' });
          break;
        case 'keepalive':
          sendResponse(await keepalive(true));
          break;
        case 'sync':
          sendResponse(await sync('host'));
          break;
        case 'status': {
          const c = await cfg();
          const map = await readGoogleCookies();
          const cd = cooldownMs(c);
          sendResponse({
            ok: true, config: c, logged_in: isLoggedIn(map), cookie_count: map.size,
            cooldown_left_ms: Math.max(0, cd - (Date.now() - (Number(c.lastRefreshAt) || 0))),
          });
          break;
        }
        default:
          sendResponse({ ok: false, detail: 'unknown type' });
      }
      await ensureAlarms();
    } catch (e) {
      sendResponse({ ok: false, detail: String((e && e.message) || e) });
    }
  })();
  return true;
});
