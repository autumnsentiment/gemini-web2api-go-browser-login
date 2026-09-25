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
 *     · 每次刷新后进入 refreshCooldownSec（默认 120 秒）冷却窗口，
 *       窗口内只做 cookie 提取，绝不做任何导航；
 *     · 只有冷却结束、且仍未拿到可用 cookie 时，才允许再刷新一次。
 *
 * 配置存在 chrome.storage.local：
 *   controller         控制器地址，默认 http://127.0.0.1:9280
 *   profile            本 profile 在池中的标识，默认 browser1
 *   token              控制器共享密钥（可空）
 *   refreshCooldownSec 刷新冷却窗口（秒），默认 120
 *   autoKeepalive / keepalivePeriodMin / autoSyncMin / enabled
 */

'use strict';

const DEFAULTS = {
  controller: 'http://127.0.0.1:9280',
  // pushMode: controller = 推给同机/同网控制器；service = 直推 gemini-web2api
  // 服务端（远程场景必需，端点 /api/browser/ingest，用 Bearer API key 鉴权）。
  pushMode: 'controller',
  profile: '',
  token: '',
  enabled: true,
  autoKeepalive: true,
  keepalivePeriodMin: 10,
  autoSyncMin: 30,
  statusPeriodMin: 5,
  refreshCooldownSec: 120,
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
  // 固定抓取页：多账号登录时，Google 用 cookie 的 path 区分账号
  // （默认账号 /，第二个 /u/1/，第三个 /u/2/…）。固定某个 Gemini 页面后，
  // 抓取一律按该页面的账号槽位取值，不再把多个账号的 cookie 混在一起。
  pinnedTabId: 0,
  pinnedUrl: '',
  pinnedAccount: '',     // '' = 默认账号；'1' / '2' … = /u/N/ 槽位
  pinnedAt: 0,
  lastStatusSyncAt: 0,
  lastStatusSyncOk: false,
  lastStatusSyncDetail: '',
};

const ALARM_KEEPALIVE = 'gw2a-keepalive';
const ALARM_SYNC = 'gw2a-sync';
const ALARM_STATUS = 'gw2a-status';
const GEMINI_URL = 'https://gemini.google.com/app';
// 尾部斜杠可选（https://gemini.google.com 不带斜杠也常出现），
// 否则会对「已存在的 Gemini 页」判 false，转去 tabs.update 甚至新建。
const GEMINI_URL_RE = /^https:\/\/gemini\.google\.com(\/|$)/;

// 多账号页面：https://gemini.google.com/u/1/app 的第二个账号槽位是 1。
// ★ 2026-09-24 实测修正 ★ Google 多账号**共用同一份 cookie**，切号靠 URL 的
// /u/N/ 路径，不是 cookie path：分别打开 /app 与 /u/1/app 两页读 cookie，
// SID/SAPISID/__Secure-1PSID 等逐项 sha1 完全相同（只有 __Secure-1PSIDTS /
// SIDCC 两张短命票不同）。所以「按 cookie path 过滤出某个账号」从根上不成立。
// 真正区分账号的做法是：把槽位随 cookie 一起上报，由服务端在请求上游时拼上
// 对应的 /u/N/ 前缀（见 payload.account / profile 的 -uN 后缀）。
// 下面的 pathCovers 过滤保留作无害兜底（正常情况下不会命中，也不会误伤）。
const GEMINI_ACCOUNT_RE = /^https:\/\/gemini\.google\.com\/u\/(\d+)(\/|$)/;

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
  const s = Number((c && c.refreshCooldownSec) || 0) || 120;
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

/**
 * 实际入池用的标识。
 *
 * 多账号时每个账号必须落成池子里**不同**的条目，否则后抓的会把先抓的覆盖掉。
 * 非默认账号（/u/N/）自动加 `-uN` 后缀；默认账号保持原样，老配置不受影响。
 * 弹窗里会显示这个最终标识，不做隐式改名。
 *
 * slotArg 显式传入时用它（「读取此页」要按那一页自己的槽位入池，而不是
 * 当前固定页的槽位）；不传则取配置里的固定槽位。
 */
async function effectiveProfile(slotArg) {
  const base = await profileName();
  const c = await cfg();
  const slot = String(slotArg !== undefined ? slotArg : (c.pinnedAccount || '')).trim();
  if (!slot) return base;
  const suffix = '-u' + slot;
  return base.endsWith(suffix) ? base : (base + suffix);
}

// ---------------------------------------------------------------- cookies

/** 站点列表：逐项 get() 时依次尝试，能命中登录 cookie 就算成功。 */
const READ_URLS = [
  'https://gemini.google.com/',
  'https://accounts.google.com/',
  'https://www.google.com/',
  'https://google.com/',
];

/** 从固定抓取页 URL 取出账号槽位：'' 表示默认账号，否则返回 '1'/'2'… */
function accountSlotFromUrl(url) {
  const m = GEMINI_ACCOUNT_RE.exec(url || '');
  return m ? m[1] : '';
}

/** 把固定抓取页 URL 换算成同槽位的「干净」Gemini 首页 URL。 */
function geminiUrlForSlot(slot) {
  const s = String(slot || '').trim();
  return s ? ('https://gemini.google.com/u/' + s + '/app') : GEMINI_URL;
}

/** cookie 的 path 是否覆盖指定页面 URL；越具体（path 越长）越接近当前账号。 */
function pathCovers(path, url) {
  if (!url) return false;
  const p = path || '/';
  if (p === '/') return true;
  const u = url.replace(/^https?:\/\//, '').split('/').slice(1).join('/') || '';
  return u === p.slice(1) || u.startsWith(p.slice(1).replace(/\/$/, '') + '/');
}

/**
 * 读取 google 域下 cookie，返回 name -> cookie 对象。
 *
 * scopedUrl 传「固定抓取页」URL 时，只挑 path 覆盖该页面的 cookie，并且同名的
 * 值优先取 path 更具体的那个。这样多账号登录时不会把 /u/1/、/u/2/ 的 SID /
 * SAPISID 混进默认账号的取值里（默认只传 scopedUrl 时，path=/ 和 path=/u/N/
 * 各有同名 cookie，必须按 path 精确区分）。
 *
 * 为什么不能只用 getAll()：Chromium 152 的 chrome.cookies.getAll({domain})
 * 会漏掉 SID / HSID / APISID 这类老式登录 cookie（实测只返回 19 条，
 * 缺的正好是判定登录所必需的那几个），于是 isLoggedIn() 永远为 false。
 * 但 chrome.cookies.get({url, name}) 能逐项取到它们（实测 SID len=153）。
 * 所以：getAll 拿全量打底，再用 get() 把 COOKIE_ORDER 里缺的逐个补齐。
 */
async function readGoogleCookies(scopedUrl) {
  return readGoogleCookiesStrict(scopedUrl);
}

/** readGoogleCookies 的实现体；scopedUrl 为空表示不按 path 过滤。 */
async function readGoogleCookiesStrict(scopedUrl) {
  const seen = new Map();
  const score = (x) => (x.domain === '.google.com' ? 2 : 0) + (x.httpOnly ? 2 : 0) + (x.secure ? 1 : 0);
  const put = (ck) => {
    if (!ck || !ck.name) return;
    // 多账号：只收 path 覆盖固定页的 cookie。浏览器本来就不会把 /u/2/ 的
    // cookie 发给 /u/1/app，这里把 domain/全量 getAll 多带出来的那些挡掉，
    // 否则会把别的账号的同名会话混进来。
    if (scopedUrl && !pathCovers(ck.path, scopedUrl)) return;
    const prev = seen.get(ck.name);
    if (!prev) { seen.set(ck.name, ck); return; }
    if (!prev.value && ck.value) { seen.set(ck.name, ck); return; }
    if (scopedUrl) {
      // 多账号：path 覆盖固定页的 cookie 优先；都覆盖时 path 越长越具体
      const a = (prev.path || '/').length;
      const b = (ck.path || '/').length;
      if (a !== b) { seen.set(ck.name, a < b ? ck : prev); return; }
    }
    if (score(ck) > score(prev)) seen.set(ck.name, ck);
  };
  const push = (list) => { for (const ck of list) put(ck); };

  const urls = scopedUrl ? [scopedUrl, ...READ_URLS] : READ_URLS;
  try { push(await chrome.cookies.getAll({ domain: '.google.com' })); } catch (e) { log('getAll google.com failed', e); }
  for (const u of urls) {
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
      for (const u of urls) {
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

/**
 * 当前抓取作用域：返回固定抓取页对应的 Gemini URL（含账号槽位）。
 *
 * 多账号登录时同一个 cookie 名字在 /、/u/1/、/u/2/ 下各有一份，只有按页面
 * URL 取值才能保证抓到的是「固定页那个账号」的会话。没固定页面时返回 ''，
 * 保持旧的「全 profile 取一份」行为。
 */
async function scopeUrl() {
  const c = await cfg();
  if (!(Number(c.pinnedTabId) || 0) && !c.pinnedAccount) return '';
  // 优先用固定页当前 URL（槽位最准确）；页面没了就用记住的槽位拼一个。
  const pinned = Number(c.pinnedTabId) || 0;
  if (pinned) {
    try {
      const t = await chrome.tabs.get(pinned);
      if (t && GEMINI_URL_RE.test(t.url || '')) return t.url;
    } catch (e) { /* 已关闭，退回槽位拼 URL */ }
  }
  return geminiUrlForSlot(c.pinnedAccount);
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

// GEMINI_ANY_RE 匹配所有「该算作 Gemini 会话页」的 URL：正式站点、以及被
// Google 风控重定向后的落地页。
var GEMINI_ANY_RE = /^https:\/\/(gemini\.google\.com|(www\.)?google\.com\/sorry)\//;

/**
 * 找到「本 profile 的那个 Gemini 标签页」，没有则返回 null（**不创建**）。
 *
 * ★ 2026-09-17 修复「一直新建页面而不是刷新」★
 *
 * 旧实现用 chrome.tabs.query({url:['https://gemini.google.com/*']}) —— 这个
 * URL 过滤有两个致命陷阱，都会让它查不到明明存在的 Gemini 标签页：
 *   1. 标签页正在加载时（status=loading）URL 尚未确定，过滤匹配不上；
 *   2. 页面被风控重定向到 www.google.com/sorry 后不再匹配 gemini 域。
 * 查不到 → 判成「没有 Gemini 页」→ chrome.tabs.create() 又开一个 —— 每次
 * 刷新都多一个窗口，这正是用户看到的「一直创建新页面」。
 *
 * 新实现不用 URL 过滤：查**全部**标签页再本地正则匹配，并依次尝试
 * ① 上次记录的 tabId（最可靠，无论停在哪个 URL 都认）
 * ② URL 匹配 gemini 域或 sorry 落地页
 * ③ 兜底：任何 url 为空的标签页（正在加载的新页也可能属于我们）
 */
async function findGeminiTab() {
  const c = await cfg();
  // ⓪ 固定抓取页优先：用户指定了就用它，不再自动挑「最近访问」的页面。
  const pinned = Number(c.pinnedTabId) || 0;
  if (pinned) {
    try {
      const t = await chrome.tabs.get(pinned);
      if (t && t.id != null) return t;
    } catch (e) { /* 固定页被关掉了，退回自动挑选 */ }
  }
  // ① 上次记录的 tabId（最可靠：无论它现在停在哪个 URL 都认）
  const remembered = Number(c.lastTabId) || 0;
  if (remembered) {
    try {
      const t = await chrome.tabs.get(remembered);
      if (t && t.id != null) return t;
    } catch (e) { /* 已被关掉 */ }
  }
  // ② 查全部标签页本地匹配（避免 URL 过滤在 loading 状态下漏判）
  let all = [];
  try { all = await chrome.tabs.query({}); } catch (e) { return null; }
  const matched = all.filter((t) => t && t.id != null && GEMINI_ANY_RE.test(t.url || ''));
  if (matched.length) {
    // 固定了账号槽位时，优先挑同一槽位的页面，避免复用到别的账号再被导航过去。
    const slot = String(c.pinnedAccount || '').trim();
    if (slot) {
      const same = matched.filter((t) => accountSlotFromUrl(t.url) === slot);
      if (same.length) {
        same.sort((a, b) => (b.lastAccessed || 0) - (a.lastAccessed || 0));
        return same[0];
      }
    }
    matched.sort((a, b) => (b.lastAccessed || 0) - (a.lastAccessed || 0));
    return matched[0];
  }
  return null;
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
    // 指定了账号槽位却没有同槽位的页面：直接开一个该槽位的新页，
    // 绝不复用别的账号的页面 —— 那会把用户正在用的页导航走，也会串号。
    const slot = String(c.pinnedAccount || '').trim();
    let anyTab = null;
    if (!slot) {
      // ★ 2026-09-17：未指定槽位时再做一次「宽口径」检查 ★
      // 只有确认整个浏览器里**一个标签页都没有**（全新窗口）时才创建；
      // 只要有任何标签页，就复用它导航到 gemini —— 用户看到的是「页面跳转」
      // 而不是「又开一个窗口」。这是「一直新建页面」的最后一道保险：
      // findGeminiTab 依赖 URL 匹配，而 URL 在 loading 状态下可能为空。
      try {
        const all = await chrome.tabs.query({});
        anyTab = (all || []).find((t) => t && t.id != null) || null;
      } catch (e) { /* ignore */ }
    }
    if (anyTab) {
      tab = anyTab;
      created = false; // 复用现有标签页，只是把 URL 指过去
    } else {
      try {
        tab = await chrome.tabs.create({ url: geminiUrlForSlot(c.pinnedAccount), active: false, pinned: true });
        created = true;
      } catch (e) {
        return { refreshed: false, error: '创建标签页失败: ' + e.message };
      }
    }
  }

  // 先记时间戳：整个加载 + 冷却窗口内都不允许再次刷新
  await setCfg({ lastRefreshAt: Date.now() });

  try {
    if (!created) {
      const isPinned = (Number(c.pinnedTabId) || 0) === tab.id;
      // 固定抓取页：无论它当前停在哪，一律拉回该账号槽位的 Gemini 页面。
      // 非固定场景维持原行为（已在 Gemini 页就 reload，不新开窗口）。
      const target = isPinned
        ? geminiUrlForSlot(c.pinnedAccount)
        : (GEMINI_URL_RE.test(tab.url || '') ? '' : GEMINI_URL);
      if (target && tab.url !== target) await chrome.tabs.update(tab.id, { url: target });
      else await chrome.tabs.reload(tab.id);
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
  const slotNow = accountSlotFromUrl(finalUrl);
  await setCfg({
    lastTabId: tab.id,
    lastTabUrl: finalUrl,
    // 固定页被关掉后由定时保活重开的同槽位页面，自动接回固定关系，
    // 否则「账号槽位还在、但没有固定页」会一直退回自动挑选。
    ...((Number(c.pinnedTabId) || 0) === 0 && c.pinnedAccount && slotNow === String(c.pinnedAccount)
      ? { pinnedTabId: tab.id, pinnedUrl: finalUrl, pinnedAt: Date.now() } : {}),
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
  const scoped = await scopeUrl();
  let map = await readGoogleCookies(scoped);
  if (isLoggedIn(map)) return { map, waited_ms: 0, exhausted: false };
  while (Date.now() < deadline) {
    await sleep(3000);
    map = await readGoogleCookies(scoped);   // 每次调用都会刷新 SW 的空闲计时
    if (isLoggedIn(map)) {
      return { map, waited_ms: budget - Math.max(0, deadline - Date.now()), exhausted: false };
    }
  }
  return { map, waited_ms: budget, exhausted: true };
}

/**
 * 保活：刷新 gemini 页面（受 120 秒冷却约束），并在页面里探活，
 * 让服务端下发新的 __Secure-1PSIDTS/SIDCC（浏览器会自动写回 cookie store）。
 */
async function keepalive(manual) {
  const c = await cfg();
  if (!c.enabled && !manual) return { ok: false, detail: '扩展已停用' };
  if (!acquireBusy()) return { ok: false, detail: '上一轮任务仍在进行' };
  try {
    // 注意：即便手动触发也不绕过 120 秒冷却，否则连续点按会连环刷新页面
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

    const map = await readGoogleCookies(await scopeUrl());
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

// ---------------------------------------------------------------- pinned page

/**
 * 列出当前打开的所有 Gemini 页面，供弹窗「读取当前页」挑选。
 *
 * 多账号登录时一个浏览器里可能同时开着 /app、/u/1/app、/u/2/app 三个页面，
 * 每个页面对应一个 Google 账号。这里把槽位、登录态、cookie 数一并算好，
 * 用户直接选「哪一页」即可，不用自己去理解 /u/N/ 的含义。
 */
async function listGeminiTabs() {
  let all = [];
  try { all = await chrome.tabs.query({}); } catch (e) { return []; }
  const out = [];
  for (const t of all) {
    if (!t || t.id == null) continue;
    if (!GEMINI_URL_RE.test(t.url || '')) continue;
    const slot = accountSlotFromUrl(t.url);
    let logged = false;
    let count = 0;
    try {
      const map = await readGoogleCookies(t.url);
      logged = isLoggedIn(map);
      count = map.size;
    } catch (e) {}
    out.push({
      tabId: t.id,
      url: t.url,
      title: t.title || '',
      account: slot,                 // '' = 默认账号
      account_label: slot ? ('账号槽位 ' + slot) : '默认账号',
      active: !!t.active,
      logged_in: logged,
      cookie_count: count,
    });
  }
  out.sort((a, b) => Number(a.account || 0) - Number(b.account || 0));
  return out;
}

/**
 * 本机登录的 Google 账号槽位清单（0..9）。
 *
 * ★ 2026-09-25 二次修正 ★
 *
 * 不能用 chrome.cookies.get 判断槽位：多账号共用同一份 cookie jar，
 * /app 和 /u/1/app 返回的是**同一条** __Secure-1PSID（domain=.google.com，
 * path=/），会 0..9 全部误判成已登录。
 *
 * 可靠判据是「页面落点」：登录了槽位 N 时，打开 /u/N/app 停在该页；
 * 没登录槽位 N 时，Google 会重定向到 /SignIn 或 /u/0/ 选择页。
 * 所以用 chrome.tabs.create({active:false}) 逐个探（后台标签，不打扰用户），
 * 加载完读 URL 判定，探完立刻关掉。仅探测 0 和已存在的 /u/N/ 页面槽位，
 * 避免每次定时任务白开 8 个标签页触发风控。
 */
async function listAccountSlots() {
  const found = new Map();   // slot -> url

  // ① 已存在的 Gemini 页面是最直接的证据，且无需新开标签
  let all = [];
  try { all = await chrome.tabs.query({}); } catch (e) {}
  for (const t of all) {
    if (!t || !GEMINI_URL_RE.test(t.url || '')) continue;
    found.set(accountSlotFromUrl(t.url), t.url);
  }
  if (!found.has('')) found.set('', GEMINI_URL);   // 槽位 0 永远要探

  // ② 对不在列表里的槽位用后台标签探测（最多补 3 个，避免连环开页）
  let probed = 0;
  for (let n = 1; n <= 3 && probed < 3; n++) {
    const key = String(n);
    if (found.has(key)) continue;
    probed += 1;
    const url = geminiUrlForSlot(key);
    let tab = null;
    try {
      tab = await chrome.tabs.create({ url, active: false });
      await waitTabComplete(tab.id, 12000);
      const t = await chrome.tabs.get(tab.id);
      const final = (t && t.url) || '';
      // 还停在 /u/N/ 说明该槽位可访问（已登录第 N 个账号）；
      // 被重定向到 accounts.google.com / /u/0/ 则说明槽位不存在。
      const m = GEMINI_ACCOUNT_RE.exec(final || '');
      if (m && m[1] === key) found.set(key, final);
      else if (GEMINI_URL_RE.test(final) && !m) found.set('', final);
    } catch (e) {
      log('probe slot', key, 'failed:', e && e.message);
    } finally {
      if (tab && tab.id != null) { try { await chrome.tabs.remove(tab.id); } catch (e) {} }
    }
  }

  const out = [];
  for (const [slot, url] of found) {
    out.push({
      account: slot,
      account_label: slot ? ('账号槽位 ' + slot) : '默认账号',
      url,
    });
  }
  out.sort((a, b) => Number(a.account || 0) - Number(b.account || 0));
  return out;
}

/**
 * 固定某个 Gemini 页面作为唯一抓取源。
 *
 * tabId 传 0 / null 表示取消固定，回到「自动挑最近访问的 Gemini 页」。
 * 固定后：
 *   · findGeminiTab / refreshGeminiTab 只认这一页，刷新也只刷它；
 *   · 读 cookie 时按该页 URL 的账号槽位过滤 path，不会串到别的账号；
 *   · 其他 Gemini 页加载完成不再触发自动入池。
 */
async function pinGeminiTab(tabId) {
  const id = Number(tabId) || 0;
  if (!id) {
    await setCfg({ pinnedTabId: 0, pinnedUrl: '', pinnedAccount: '', pinnedAt: 0 });
    return { ok: true, pinned: false, detail: '已取消固定抓取页，回到自动选择' };
  }
  let tab;
  try {
    tab = await chrome.tabs.get(id);
  } catch (e) {
    return { ok: false, detail: '页面已关闭，无法固定' };
  }
  if (!tab || !GEMINI_URL_RE.test(tab.url || '')) {
    return { ok: false, detail: '该页面不是 Gemini 页面，无法固定' };
  }
  const slot = accountSlotFromUrl(tab.url);
  await setCfg({
    pinnedTabId: id,
    pinnedUrl: tab.url || '',
    pinnedAccount: slot,
    pinnedAt: Date.now(),
    lastTabId: id,
    lastTabUrl: tab.url || '',
  });
  return {
    ok: true, pinned: true, tabId: id, url: tab.url, account: slot,
    detail: '已固定抓取页（' + (slot ? '账号槽位 ' + slot : '默认账号') + '）',
  };
}

/**
 * 「读取当前页」：读取指定 Gemini 页面对应账号的 cookie 并立即入池。
 *
 * 与 sync() 的区别：不刷新页面、不看冷却窗口，纯只读 —— 页面已经登录时
 * 直接取值即可（用户明确要求的路径：打开 Gemini 页面、不发消息直接读）。
 */
async function readPageAndSync(tabId) {
  const id = Number(tabId) || 0;
  if (!id) return { ok: false, detail: '没有指定页面' };
  let tab;
  try {
    tab = await chrome.tabs.get(id);
  } catch (e) {
    return { ok: false, detail: '页面已关闭' };
  }
  if (!tab || !GEMINI_URL_RE.test(tab.url || '')) {
    return { ok: false, detail: '该页面不是 Gemini 页面' };
  }
  // 手动「读取此页」优先级最高：定时保活/自动同步只做读 cookie + 推送，
  // 不像 readPage 这样需要立刻给用户结果。若锁被占，等最多 8 秒让上一轮
  // 结束（正常保活几秒就完），超时才报忙 —— 避免浏览器刚唤醒时
  // onStartup 的 keepalive 占着锁，用户点「读取此页」直接被打回。
  const deadline = Date.now() + 8000;
  while (!acquireBusy()) {
    if (Date.now() >= deadline) return { ok: false, detail: '上一轮任务仍在进行' };
    await sleep(200);
  }
  try {
    const c = await cfg();
    const map = await readGoogleCookies(tab.url);
    if (!isLoggedIn(map)) {
      const r = { ok: false, detail: '该页面未登录（缺少 SID/SAPISID/__Secure-1PSID）' };
      await setCfg({ lastSyncAt: Date.now(), lastSyncOk: false, lastSyncDetail: r.detail });
      return r;
    }
    return await pushCookie(map, {
      reason: 'read-page',
      url: tab.url,
      account: accountSlotFromUrl(tab.url),
      config: c,
    });
  } finally {
    releaseBusy();
  }
}

/**
 * 「抓取全部账号」：枚举本机所有已登录的 Google 账号槽位，逐个槽位读
 * cookie 并按各自槽位入池（profile 带 -uN 后缀），不刷新任何页面。
 *
 * 2026-09-25 修复：旧逻辑只认「固定抓取页」，本机登录的第二个账号如果
 * 没开 /u/1/ 页面就永远入不了池。现在即使一个 Gemini 页面都没开，
 * 只要 cookie jar 里有该槽位的会话票，就能抓到。
 */
async function readAllAccountsAndSync() {
  if (!acquireBusy()) return { ok: false, detail: '上一轮任务仍在进行' };
  try {
    const c = await cfg();
    const slots = await listAccountSlots();
    if (!slots.length) {
      const r = { ok: false, detail: '本机没有已登录的 Google 账号（cookie jar 里没有任何槽位的会话票）' };
      await setCfg({ lastSyncAt: Date.now(), lastSyncOk: false, lastSyncDetail: r.detail });
      return r;
    }
    const results = [];
    for (const s of slots) {
      const map = await readGoogleCookies(s.url);
      if (!isLoggedIn(map)) {
        results.push({ account: s.account, ok: false, detail: '槽位 ' + s.account + ' 缺会话 cookie' });
        continue;
      }
      const pr = await pushCookie(map, {
        reason: 'read-all',
        url: s.url,
        account: s.account,
        config: c,
      });
      results.push({
        account: s.account,
        ok: pr.ok,
        id: pr.body && pr.body.id,
        detail: pr.detail || (pr.body && pr.body.detail) || '',
      });
    }
    const okCount = results.filter((x) => x.ok).length;
    const detail = results.map((x) =>
      (x.ok ? '✓' : '✗') + (Number(x.account) ? 'u' + x.account : '默认') +
      (x.id ? '#' + x.id : '')).join(' ');
    const r = { ok: okCount > 0, count: okCount, total: results.length, results, detail };
    await setCfg({
      lastSyncAt: Date.now(),
      lastSyncOk: r.ok,
      lastSyncDetail: '全部账号入池 ' + okCount + '/' + results.length + '：' + detail,
    });
    return r;
  } finally {
    releaseBusy();
  }
}

// ---------------------------------------------------------------- sync

/**
 * 把一份 cookie 推送到控制器/服务端，由对方写入容器内 cookie 池。
 *
 * opts:
 *   reason    触发原因，写进 payload 便于排查
 *   url       这份 cookie 来自哪个页面（含账号槽位，供服务端记录）
 *   account   账号槽位（'' = 默认账号）
 *   refreshed 本轮是否刷新过页面
 *   config    已读好的配置（不传则现读）
 *
 * 推送目标（两种模式）：
 *   controller → http://<controller>:9280/cookie-sync（无鉴权，靠内网限制）
 *   service    → http://<server>:8083/api/browser/ingest（Bearer API key 鉴权）
 */
async function pushCookie(map, opts) {
  const o = opts || {};
  const c = o.config || await cfg();
  const { header } = buildCookieHeader(map);
  const sapisid = (map.get('SAPISID') || {}).value || '';
  const ts = Math.floor(Date.now() / 1000);
  const origin = 'https://gemini.google.com';
  const sapisidhash = sapisid
    ? ts + '_' + (await sha1Hex(ts + ' ' + sapisid + ' ' + origin))
    : '';
  // 入池标识按「这份 cookie 所属的账号槽位」算：readPage 传的是那一页自己的槽位，
  // 定时 sync 没传就取配置里的固定槽位，两者不会互相串。
  const profile = await effectiveProfile(o.account);

  const payload = {
    profile,
    push_mode: (c.pushMode === 'service') ? 'service' : 'controller',
    reason: o.reason || 'auto',
    url: o.url || GEMINI_URL,
    account: o.account || '',
    ts,
    cookie: header,
    sapisidhash,
    refreshed: !!o.refreshed,
    rotated: Date.now() - (Number(c.lastRotateAt) || 0) < 120000,
    logged_in: true,
    user_agent: navigator.userAgent,
    summary: cookieSummary(map),
    client: 'gw2a-ext/1.1.3',
  };

  const mode = (c.pushMode === 'service') ? 'service' : 'controller';
  const base = (c.controller || '').replace(/\/+$/, '');
  const url = mode === 'service' ? (base + '/api/browser/ingest') : (base + '/cookie-sync');
  let r;
  try {
    const headers = { 'Content-Type': 'application/json', 'X-GW2A-Ext-Mode': mode };
    if (mode === 'service') {
      if (c.token) headers['Authorization'] = 'Bearer ' + c.token;
    } else if (c.token) {
      headers['X-GW2A-Token'] = c.token;
    }
    const resp = await fetch(url, { method: 'POST', headers, body: JSON.stringify(payload) });
    const text = await resp.text();
    let body = null;
    try { body = JSON.parse(text); } catch (e) { body = { raw: text }; }
    r = { ok: resp.ok && !(body && body.error), http: resp.status, body, mode };
    if (!r.ok) {
      const why = (body && (body.error || body.detail)) || text.slice(0, 120);
      r.detail = (mode === 'service' ? '服务端返回 ' : '控制器返回 ') + resp.status + ' ' + why;
    } else {
      r.detail = (body && body.detail) || '已入池';
      if (body && body.verify && body.verify.ok === false) {
        r.detail += ' · 校验未通过：' + (body.verify.detail || '');
      }
    }
  } catch (e) {
    r = { ok: false, detail: '推送失败: ' + e.message + '（目标 ' + url + '）' };
  }

  await setCfg({
    lastSyncAt: Date.now(),
    lastSyncOk: !!r.ok,
    lastSyncDetail: r.detail || '',
  });
  log('pushCookie', r);
  return r;
}

/**
 * 定时入池。
 *
 * 节奏（风控友好）：
 *   1) 距上次刷新 ≥ 120s → 刷新一次页面，然后在 120s 冷却窗口里轮询提取；
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
    // ★ 2026-09-25：多账号全量入池 ★
    // 服务端模式下本机浏览器可能同时登录多个 Google 账号，只抓「固定页」
    // 的槽位会让其他账号永远入不了池。定时同步改为遍历所有已登录槽位，
    // 每个槽位各自入池（profile 带 -uN 后缀，服务端按 /u/N/ 请求）。
    // 仍然尊重冷却窗口：本轮已成功入过池的槽位直接跳过，避免高频推送。
    const slots = await listAccountSlots();
    if (!slots.length) {
      const r = { ok: false, detail: '未登录（没有任何账号槽位的会话 cookie）' };
      await setCfg({ lastSyncAt: Date.now(), lastSyncOk: false, lastSyncDetail: r.detail });
      return r;
    }
    const results = [];
    for (const s of slots) {
      const scoped = s.url;
      let map = await readGoogleCookies(scoped);
      let logged = isLoggedIn(map);

      if (!logged || since >= cd) {
      // 冷却已过（或当前读不到登录 cookie）：允许刷新一次
      // 一律不 force：手动触发也受 120 秒冷却约束，防止连点造成连环刷新
        const r = await refreshGeminiTab(false);
        const refreshed = !!r.refreshed;   // 冷却未过时 r.refreshed=false，只提取不导航
        if (refreshed) {
          const got = await collectWithinCooldown(cd);
          map = got.map;
          logged = isLoggedIn(map);
          if (got.exhausted && !logged) log('冷却窗口内仍未取到登录 cookie');
        } else {
          map = await readGoogleCookies(scoped);
          logged = isLoggedIn(map);
        }
      } else {
        log('冷却中，跳过刷新，直接提取 cookie（剩余 ' +
            Math.ceil((cd - since) / 1000) + 's）');
      }

      if (!map.size || !logged) {
        results.push({ account: s.account, ok: false, detail: '槽位 ' + s.account + ' 未读到登录 cookie' });
        continue;
      }

      // 登录态 OK 时顺手触发一次轮换（60s 内只做一次），再重读
      if (Date.now() - (Number(c.lastRotateAt) || 0) > 60000) {
        await forceRotateCookies();
        await setCfg({ lastRotateAt: Date.now() });
        await sleep(800);                     // 等 Set-Cookie 落盘
        map = await readGoogleCookies(scoped);
      }

      const pr = await pushCookie(map, {
        reason: reason || 'auto',
        url: scoped,
        account: s.account,
        refreshed: false,
        config: c,
      });
      results.push({ account: s.account, ok: pr.ok, id: pr.body && pr.body.id });
    }
    const okCount = results.filter((x) => x.ok).length;
    const detail = results.map((x) =>
      (x.ok ? '✓' : '✗') + (Number(x.account) ? 'u' + x.account : '默认') +
      (x.id ? '#' + x.id : '')).join(' ');
    const r = { ok: okCount > 0, count: okCount, total: results.length, results, detail };
    await setCfg({
      lastSyncAt: Date.now(),
      lastSyncOk: r.ok,
      lastSyncDetail: '多账号入池 ' + okCount + '/' + results.length + '：' + detail,
    });
    return r;
  } finally {
    if (own) releaseBusy();
  }
}

/**
 * 状态同步：把「扩展存活 + 当前推送模式」上报给服务端。
 *
 * controller 模式 → POST <controller>/extension-status（宿主机控制器写容器 kv）
 * service 模式    → POST <server>/api/browser/extension-status（Bearer API key）
 *
 * 面板的「同步插件状态」按钮依赖这条心跳判断插件存活；扩展弹窗里的
 * 「同步状态」按钮也调用它，用于用户主动发起同步。
 */
async function syncStatus(reason) {
  const c = await cfg();
  const mode = (c.pushMode === 'service') ? 'service' : 'controller';
  const base = (c.controller || '').replace(/\/+$/, '');
  const url = mode === 'service'
    ? (base + '/api/browser/extension-status')
    : (base + '/extension-status');
  let map = new Map();
  try { map = await readGoogleCookies(await scopeUrl()); } catch (e) {}
  // 心跳也要用带槽位的标识，否则固定到 /u/N/ 后面板里对应那一行永远收不到状态
  const profile = await effectiveProfile();
  const payload = {
    profile,
    push_mode: mode,
    logged_in: isLoggedIn(map),
    cookie_count: map.size,
    detail: '扩展主动状态同步（' + (reason || 'manual') + '）',
    client: 'gw2a-ext/1.1.3',
    ts: Math.floor(Date.now() / 1000),
  };
  let r;
  try {
    const headers = { 'Content-Type': 'application/json', 'X-GW2A-Ext-Mode': mode };
    if (mode === 'service' && c.token) headers['Authorization'] = 'Bearer ' + c.token;
    else if (mode === 'controller' && c.token) headers['X-GW2A-Token'] = c.token;
    const resp = await fetch(url, { method: 'POST', headers, body: JSON.stringify(payload) });
    const text = await resp.text();
    let body = null;
    try { body = JSON.parse(text); } catch (e) { body = { raw: text }; }
    r = { ok: resp.ok && !(body && body.error), http: resp.status, body, mode };
    r.detail = r.ok
      ? ('状态已同步（' + mode + '）')
      : ('状态同步失败：HTTP ' + resp.status + ' ' +
         ((body && (body.error || body.detail)) || text.slice(0, 100)));
  } catch (e) {
    r = { ok: false, mode, detail: '状态同步失败: ' + e.message + '（目标 ' + url + '）' };
  }
  await setCfg({
    lastStatusSyncAt: Date.now(),
    lastStatusSyncOk: !!r.ok,
    lastStatusSyncDetail: r.detail || '',
  });
  log('syncStatus', r);
  return r;
}

// ---------------------------------------------------------------- alarms

async function ensureAlarms() {
  const c = await cfg();
  await chrome.alarms.clear(ALARM_KEEPALIVE);
  await chrome.alarms.clear(ALARM_SYNC);
  await chrome.alarms.clear(ALARM_STATUS);
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
  chrome.alarms.create(ALARM_STATUS, {
    delayInMinutes: 0.2,
    periodInMinutes: Math.max(1, Number(c.statusPeriodMin) || 5),
  });
}

chrome.alarms.onAlarm.addListener(async (a) => {
  try {
    if (a.name === ALARM_STATUS) {
      await syncStatus('heartbeat');
    } else if (a.name === ALARM_KEEPALIVE) {
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
  try { await syncStatus('installed'); } catch (e) {}
  try { await sync('installed'); } catch (e) {}
});

chrome.runtime.onStartup.addListener(async () => {
  await ensureAlarms();
  try { await syncStatus('startup'); } catch (e) {}
  try { await keepalive(false); } catch (e) {}
});

// gemini 页面加载完成时顺手同步（登录后立刻入池）。
// 注意：sync 内部有 60 秒冷却保护，这里不会引起连环刷新。
chrome.tabs.onUpdated.addListener(async (tabId, info, tab) => {
  if (info.status !== 'complete') return;
  if (!tab || !GEMINI_URL_RE.test(tab.url || '')) return;
  // 固定抓取页启用时，只认那一页，其他 Gemini 页加载完成不触发入池，
  // 否则多账号同时开着时会被别的账号的页面加载带跑。
  const pinned = Number((await cfg()).pinnedTabId) || 0;
  if (pinned && tab.id !== pinned) return;
  if (BUSY) return;                       // 自己刷新引起的加载，直接忽略
  try {
    const c = await cfg();
    if (!c.enabled) return;
    const map = await readGoogleCookies(await scopeUrl());
    if (isLoggedIn(map)) await sync('page-load');
  } catch (e) { log('onUpdated sync error', e); }
});

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    try {
      switch (msg && msg.type) {
        case 'status': {
          const c = await cfg();
          const map = await readGoogleCookies(await scopeUrl());
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
        case 'syncAll':
          sendResponse(await readAllAccountsAndSync());
          break;
        case 'syncStatus':
        case 'statusSync':
          sendResponse(await syncStatus('manual'));
          break;
        case 'listTabs':
          sendResponse({ ok: true, tabs: await listGeminiTabs() });
          break;
        case 'pinTab':
          sendResponse(await pinGeminiTab(msg.tabId));
          break;
        case 'readPage':
          sendResponse(await readPageAndSync(msg.tabId));
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

// 固定抓取页被关掉时清掉记录，避免后续一直找一个不存在的标签页。
// 不清 pinnedAccount：用户下次点「读取当前页」重新固定时再覆盖。
chrome.tabs.onRemoved.addListener(async (tabId) => {
  try {
    const c = await cfg();
    if ((Number(c.pinnedTabId) || 0) === tabId) {
      await setCfg({ pinnedTabId: 0, pinnedUrl: '', pinnedAt: 0 });
      log('固定抓取页已关闭，自动取消固定（账号槽位保留：' + (c.pinnedAccount || '默认') + '）');
    }
    if ((Number(c.lastTabId) || 0) === tabId) {
      await setCfg({ lastTabId: 0, lastTabUrl: '' });
    }
  } catch (e) { log('onRemoved cleanup error', e); }
});

// 宿主页（控制器托管的引导页）远程控制入口：
// chrome.runtime.sendMessage(EXT_ID, {...}) 即可触发保活/入池，无需用户点击。
chrome.runtime.onMessageExternal.addListener((msg, sender, sendResponse) => {
  (async () => {
    try {
      const cfgPatch = (msg && msg.patch) || null;
      if (cfgPatch) await setCfg(cfgPatch);
      switch (msg && msg.type) {
        case 'ping':
          sendResponse({ ok: true, pong: true, client: 'gw2a-ext/1.1.3' });
          break;
        case 'listTabs':
          sendResponse({ ok: true, tabs: await listGeminiTabs() });
          break;
        case 'pinTab':
          sendResponse(await pinGeminiTab(msg.tabId));
          break;
        case 'readPage':
          sendResponse(await readPageAndSync(msg.tabId));
          break;
        case 'syncStatus':
        case 'statusSync':
          sendResponse(await syncStatus('host'));
          break;
        case 'keepalive':
          sendResponse(await keepalive(true));
          break;
        case 'sync':
          sendResponse(await sync('host'));
          break;
        case 'syncAll':
          sendResponse(await readAllAccountsAndSync());
          break;
        case 'status': {
          const c = await cfg();
          const map = await readGoogleCookies(await scopeUrl());
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
