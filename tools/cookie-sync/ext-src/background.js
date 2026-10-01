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
  // 单个扩展实例只绑定一个明确的 Gemini 标签页/账号槽位。
  boundTabId: 0,
  boundTabUrl: '',
  boundAuthuser: 0,
  // Email read from the bound page itself (WIZ_global_data.oPEP7c). It is
  // re-checked before every ingest and verified again by the server.
  boundEmail: '',
  boundStoreId: '',
  boundAt: 0,
  lastReadPageAt: 0,
  lastReadPageDetail: '',
};

const CLIENT = 'gw2a-ext/1.1.6-page-identity';

const ALARM_KEEPALIVE = 'gw2a-keepalive';
const ALARM_SYNC = 'gw2a-sync';
const ALARM_STATUS = 'gw2a-status';
const GEMINI_URL = 'https://gemini.google.com/app';
// 尾部斜杠可选（https://gemini.google.com 不带斜杠也常出现），
// 否则会对「已存在的 Gemini 页」判 false，转去 tabs.update 甚至新建。
const GEMINI_URL_RE = /^https:\/\/gemini\.google\.com(\/|$)/;

// 被 Google 风控拦下时的落地页（www.google.com/sorry/...）。这种页面同样是
// 「本 profile 的那个 Gemini 标签页」，必须复用而不是另开新页。
const SORRY_URL_RE = /^https:\/\/(www\.)?google\.com\/sorry\//;

function isGeminiUrl(url) {
  return GEMINI_URL_RE.test(String(url || ''));
}

function isGeminiOrSorryUrl(url) {
  const s = String(url || '');
  return isGeminiUrl(s) || SORRY_URL_RE.test(s);
}

/**
 * Parse an explicit Google account slot from /u/N or ?authuser=N.
 * Returns null when the URL names no numeric slot (plain /app); the page
 * session index is then used instead of silently assuming the default.
 */
function authuserFromUrl(url) {
  const s = String(url || '');
  const m = s.match(/\/u\/(\d+)(?:\/|$)/) || s.match(/[?&]authuser=(\d+)/);
  if (!m) return null;
  const n = Number(m[1]);
  return Number.isInteger(n) && n >= 0 && n <= 9 ? n : null;
}

function authuserLabel(n) {
  return Number(n) > 0 ? '/u/' + Number(n) : '默认账号 /u/0';
}

function sameEmail(a, b) {
  return String(a || '').trim().toLowerCase() === String(b || '').trim().toLowerCase();
}

/**
 * Read the signed-in identity of exactly one tab from the page (MAIN world).
 * WIZ_global_data.oPEP7c is the account email, QrtxK the session index.
 * No navigation, no reload, no other tabs.
 */
async function probePageIdentity(tabId) {
  try {
    const res = await chrome.scripting.executeScript({
      target: { tabId },
      world: 'MAIN',
      func: () => {
        const w = window.WIZ_global_data || {};
        const raw = w.QrtxK == null ? '' : String(w.QrtxK);
        const slot = /^\d$/.test(raw) ? Number(raw) : null;
        let email = typeof w.oPEP7c === 'string' && w.oPEP7c.indexOf('@') > 0 ? w.oPEP7c : '';
        if (!email) {
          const nodes = document.querySelectorAll('a[aria-label*="@"], [data-email]');
          for (const el of nodes) {
            const text = (el.getAttribute('data-email') || '') + ' ' + (el.getAttribute('aria-label') || '');
            const m = text.match(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/);
            if (m) { email = m[0]; break; }
          }
        }
        const body = (document.body && document.body.innerText || '').slice(0, 1200);
        return {
          url: location.href,
          title: document.title || '',
          email,
          slot,
          has_token: typeof w.SNlM0e === 'string' && w.SNlM0e.length > 0,
          signed_out: /Sign in to save activity|^Sign in$/m.test(body),
        };
      },
    });
    return (res && res[0] && res[0].result) || { error: '页面没有返回身份信息' };
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
}

/** An explicit URL slot wins; otherwise use the page's own session index. */
function resolveSlot(url, probe) {
  const fromUrl = authuserFromUrl(url);
  if (fromUrl !== null) return { slot: fromUrl, source: 'url' };
  if (probe && Number.isInteger(probe.slot)) return { slot: probe.slot, source: 'page' };
  return { slot: 0, source: 'default' };
}

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

// ---------------------------------------------------------------- cookies

/** 站点列表：逐项 get() 时依次尝试，能命中登录 cookie 就算成功。 */
const READ_URLS = [
  'https://gemini.google.com/',
  'https://accounts.google.com/',
  'https://www.google.com/',
  'https://google.com/',
];

/** Match the same host/path rules Chrome applies to a request for this URL. */
function cookieCoversUrl(ck, url) {
  const host = url.hostname.toLowerCase();
  const domain = String(ck.domain || '').replace(/^\./, '').toLowerCase();
  if (ck.hostOnly ? host !== domain : host !== domain && !host.endsWith('.' + domain)) return false;
  if (ck.secure && url.protocol !== 'https:') return false;
  const path = ck.path || '/';
  const target = url.pathname || '/';
  return target === path || (target.startsWith(path) &&
    (path.endsWith('/') || target.charAt(path.length) === '/'));
}

/**
 * Read only cookies applicable to the bound Gemini URL and its cookie store.
 * Google accounts in one store may share root-path cookies; the bound URL's
 * /u/N slot is still required to select the account on upstream requests.
 */
async function readGoogleCookies(storeId, scopedUrl) {
  const scope = scopedUrl && isGeminiUrl(scopedUrl) ? new URL(scopedUrl) : null;
  if (scopedUrl && !scope) return new Map();
  const opts = (base) => {
    if (!storeId) return base;
    return Object.assign({}, base, { storeId: String(storeId) });
  };
  const seen = new Map();
  const score = (x) => (x.domain === '.google.com' ? 2 : 0) + (x.httpOnly ? 2 : 0) + (x.secure ? 1 : 0);
  const put = (ck) => {
    if (!ck || !ck.name) return;
    if (scope && !cookieCoversUrl(ck, scope)) return;
    const prev = seen.get(ck.name);
    if (!prev) { seen.set(ck.name, ck); return; }
    if (!prev.value && ck.value) { seen.set(ck.name, ck); return; }
    if (scope) {
      const prevLength = (prev.path || '/').length;
      const nextLength = (ck.path || '/').length;
      if (prevLength !== nextLength) {
        if (nextLength > prevLength) seen.set(ck.name, ck);
        return;
      }
    }
    if (score(ck) > score(prev)) seen.set(ck.name, ck);
  };
  const push = (list) => { for (const ck of list) put(ck); };

  const urls = scope ? [scope.href] : READ_URLS;
  try { push(await chrome.cookies.getAll(opts({ domain: '.google.com' }))); } catch (e) { log('getAll google.com failed', e); }
  for (const u of urls) {
    try { push(await chrome.cookies.getAll(opts({ url: u }))); } catch (e) {}
  }
  if (!scope) {
    try { push(await chrome.cookies.getAll(opts({}))); } catch (e) {}
  }

  // getAll can omit legacy auth cookies or a more specific path. In scoped
  // mode get({url,name}) must also run for names already seen at path=/.
  const wanted = scope ? COOKIE_ORDER : COOKIE_ORDER.filter((n) => {
    const ck = seen.get(n);
    return !ck || !ck.value;
  });
  if (wanted.length) {
    for (const name of wanted) {
      for (const u of urls) {
        try {
          const ck = await chrome.cookies.get(opts({ url: u, name }));
          if (ck && ck.value) { put(ck); break; }
        } catch (e) {}
      }
    }
    const still = wanted.filter((n) => !(seen.get(n) || {}).value);
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

/** Return the cookie store containing a tab. Chrome stores cookies per
 * profile/incognito store, not per tab; tabIds is the only reliable bridge. */
async function cookieStoreIdForTab(tabId) {
  try {
    if (chrome.cookies.getAllCookieStores) {
      const stores = await chrome.cookies.getAllCookieStores();
      for (const store of stores || []) {
        if ((store.tabIds || []).some((id) => Number(id) === Number(tabId))) {
          return String(store.id || '');
        }
      }
    }
  } catch (e) { log('getAllCookieStores failed', e); }
  return '';
}

/**
 * Return only the explicitly bound tab. There is deliberately no fallback to
 * another Gemini tab, no all-tabs scan, and no account-slot probing.
 */
async function getBoundGeminiTab() {
  const c = await cfg();
  // Only an explicit binding is authoritative. `lastTabId` is historical
  // refresh state and must never resurrect a page after unbind/reload.
  const id = Number(c.boundTabId) || 0;
  if (!id) return { ok: false, detail: '未绑定 Gemini 页面' };
  let tab;
  try {
    tab = await chrome.tabs.get(id);
  } catch (e) {
    return { ok: false, detail: '绑定页面已关闭，请重新绑定' };
  }
  const url = String((tab && tab.url) || '');
  if (!isGeminiOrSorryUrl(url)) {
    return { ok: false, detail: '绑定页面已离开 Gemini，请重新绑定', tab, url };
  }
  const currentStoreId = await cookieStoreIdForTab(id);
  const boundAuthuser = Number.isInteger(Number(c.boundAuthuser)) ? Number(c.boundAuthuser) : 0;
  // Only an explicit /u/N in the current URL can contradict the binding;
  // a plain /app URL is resolved from the page at bind/ingest time.
  const urlSlot = authuserFromUrl(url);
  const currentAuthuser = urlSlot === null ? boundAuthuser : urlSlot;
  if (currentAuthuser !== boundAuthuser) {
    return {
      ok: false,
      detail: '绑定页面账号槽位已改变（原 ' + authuserLabel(boundAuthuser) +
        '，当前 ' + authuserLabel(currentAuthuser) + '），请重新绑定',
      tab, url, authuser: currentAuthuser,
    };
  }
  const boundStoreId = String(c.boundStoreId || '');
  if (boundStoreId && currentStoreId && boundStoreId !== currentStoreId) {
    return {
      ok: false,
      detail: '绑定页面的 Cookie 存储区已改变，请重新绑定',
      tab, url, authuser: currentAuthuser, store_id: currentStoreId,
    };
  }
  return { ok: true, tab, url, authuser: boundAuthuser, email: String(c.boundEmail || ''),
    storeId: boundStoreId || currentStoreId };
}

async function findGeminiTab() {
  const bound = await getBoundGeminiTab();
  return bound.ok ? bound.tab : null;
}

/**
 * Return the one tab that was active when the popup was opened. Some Chrome
 * builds report the extension popup context as `lastFocusedWindow`, so try
 * the popup's current window first and then the last focused normal window.
 * We never query all tabs or choose an inactive Gemini page.
 */
async function getPopupActiveTab() {
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs && tabs[0] && tabs[0].id != null) return tabs[0];
  } catch (e) {}
  try {
    const tabs = await chrome.tabs.query({ active: true, lastFocusedWindow: true });
    if (tabs && tabs[0] && tabs[0].id != null) return tabs[0];
  } catch (e) {}
  try {
    const win = await chrome.windows.getLastFocused({ populate: true });
    if (win && win.type === 'normal') {
      const tab = (win.tabs || []).find((item) => item && item.active && item.id != null);
      if (tab) return tab;
    }
  } catch (e) {}
  return null;
}

/** Bind the explicit popup tab only. This function never navigates or reloads it. */
async function bindCurrentPage(request) {
  const requestedId = Number(request && (request.tabId || request.tab_id)) || 0;
  let tab = null;
  if (requestedId) {
    try {
      tab = await chrome.tabs.get(requestedId);
    } catch (e) {
      return { ok: false, detail: '弹窗传入的目标页面已关闭，请回到 Gemini 页面重试' };
    }
  } else {
    tab = await getPopupActiveTab();
  }
  const url = String((tab && tab.url) || '');
  if (!tab || tab.id == null) {
    return { ok: false, detail: '没有获取到当前活动标签页，请在 Gemini 页面重新打开扩展弹窗' };
  }
  if (!isGeminiUrl(url)) {
    return {
      ok: false,
      detail: '当前活动页面不是 Gemini 页面，未绑定（当前地址：' + (url || '不可读取') + '）',
      url,
      tab_id: tab.id,
    };
  }
  const probe = await probePageIdentity(tab.id);
  if (probe.error) {
    return { ok: false, detail: '无法读取页面账号（' + probe.error + '），请刷新该 Gemini 页后重试', url, tab_id: tab.id };
  }
  if (probe.signed_out || !probe.has_token) {
    return { ok: false, detail: '该页面未登录 Gemini（页面没有会话令牌），未绑定', url, tab_id: tab.id };
  }
  if (!probe.email) {
    return { ok: false, detail: '页面未暴露账号邮箱，无法确认是哪个账号，未绑定', url, tab_id: tab.id };
  }
  const resolved = resolveSlot(url, probe);
  if (resolved.source === 'url' && Number.isInteger(probe.slot) && probe.slot !== resolved.slot) {
    return {
      ok: false,
      detail: '页面地址是 ' + authuserLabel(resolved.slot) + '，但页面会话是 ' +
        authuserLabel(probe.slot) + '，请在该账号自己的页面上绑定',
      url, tab_id: tab.id,
    };
  }
  const authuser = resolved.slot;
  const storeId = await cookieStoreIdForTab(tab.id);
  const patch = {
    boundTabId: tab.id,
    boundTabUrl: url,
    boundAuthuser: authuser,
    boundEmail: probe.email,
    boundStoreId: storeId,
    boundAt: Date.now(),
    lastTabId: tab.id,
    lastTabUrl: url,
  };
  const c = await setCfg(patch);
  const page = await inspectBoundPage();
  return {
    ok: true, config: c, page,
    detail: '已绑定 ' + probe.email + '（' + authuserLabel(authuser) +
      (resolved.source === 'page' ? '，由页面识别' : '') + '）',
  };
}

async function unbindCurrentPage() {
  const c = await setCfg({
    boundTabId: 0,
    boundTabUrl: '',
    boundAuthuser: 0,
    boundEmail: '',
    boundStoreId: '',
    boundAt: 0,
    lastTabId: 0,
    lastTabUrl: '',
  });
  return { ok: true, config: c, detail: '已解除页面绑定' };
}

/** Read visible identity from the bound page without navigation or reload. */
async function inspectBoundPage() {
  const bound = await getBoundGeminiTab();
  if (!bound.ok) return { ok: false, detail: bound.detail, url: bound.url || '' };
  const probe = await probePageIdentity(bound.tab.id);
  const mismatch = !!(probe.email && bound.email && !sameEmail(probe.email, bound.email));
  return {
    ok: true,
    tab_id: bound.tab.id,
    url: bound.url,
    authuser: bound.authuser,
    store_id: bound.storeId || '',
    email: probe.email || '',
    bound_email: bound.email || '',
    page_slot: Number.isInteger(probe.slot) ? probe.slot : null,
    account_mismatch: mismatch,
    has_token: !!probe.has_token,
    title: probe.title || '',
    signed_out: !!probe.signed_out,
    error: probe.error || null,
  };
}

/** Read the bound page and the shared cookie jar, never refreshing the page. */
async function readCurrentPage() {
  const page = await inspectBoundPage();
  if (!page.ok) {
    await setCfg({ lastReadPageAt: Date.now(), lastReadPageDetail: page.detail || '读取失败' });
    return page;
  }
  const map = await readGoogleCookies(page.store_id || '', page.url);
  const logged = isLoggedIn(map);
  const cookie = buildCookieHeader(map);
  const fingerprint = cookie.header ? await sha1Hex(cookie.header) : '';
  const detail = page.signed_out
    ? '绑定页显示未登录'
    : page.account_mismatch
      ? '绑定页账号已变为 ' + page.email + '（绑定时是 ' + page.bound_email + '），请重新绑定'
      : '已读取绑定页，不刷新；' + (page.email || '页面未暴露邮箱') + ' · ' + authuserLabel(page.authuser);
  await setCfg({ lastReadPageAt: Date.now(), lastReadPageDetail: detail });
  return {
    ok: true,
    page,
    logged_in: logged,
    cookie_count: map.size,
    summary: cookieSummary(map),
    cookie_fingerprint: fingerprint,
    detail,
  };
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

/** Refresh only the bound tab. No binding means no navigation and no new tab. */
async function refreshGeminiTab(force) {
  const c = await cfg();
  const cd = cooldownMs(c);
  const last = Number(c.lastRefreshAt) || 0;
  const since = Date.now() - last;
  const bound = await getBoundGeminiTab();
  if (!bound.ok) return { refreshed: false, error: bound.detail, tab: bound.tab };
  if (!force && last && since < cd) {
    return {
      refreshed: false,
      cooldown_left_ms: cd - since,
      tab: bound.tab,
      authuser: bound.authuser,
    };
  }
  const tab = bound.tab;

  // 先记时间戳：整个加载 + 冷却窗口内都不允许再次刷新
  await setCfg({ lastRefreshAt: Date.now() });

  try {
    // The tab was validated above. Reloading a sorry page is still limited to
    // the same tab; we never navigate it to a different account or URL.
    await chrome.tabs.reload(tab.id);
  } catch (e) {
    await setCfg({ lastRefreshDetail: '刷新失败: ' + e.message });
    return { refreshed: false, error: '刷新失败: ' + e.message, tab };
  }

  await waitTabComplete(tab.id, 30000);
  // 记住这个标签页：下次直接复用它，绝不再新建
  let finalUrl = '';
  try { const t = await chrome.tabs.get(tab.id); finalUrl = (t && t.url) || ''; } catch (e) {}
  const sorry = SORRY_URL_RE.test(finalUrl);
  const finalSlot = authuserFromUrl(finalUrl);
  if (!sorry && finalSlot !== null && finalSlot !== bound.authuser) {
    await setCfg({ lastRefreshDetail: '刷新后页面账号槽位改变，停止自动处理' });
    return { refreshed: false, error: '刷新后页面账号槽位改变，请重新绑定', tab };
  }
  await setCfg({
    lastTabId: tab.id,
    lastTabUrl: finalUrl,
    lastRefreshDetail: '刷新绑定页面'
      + (sorry ? '（被风控重定向到 sorry 页，本轮只提取 cookie）' : '')
      + ' @ ' + new Date().toISOString(),
  });
  log('refresh bound gemini tab', tab.id, '(reload)',
      sorry ? '[sorry 页]' : '');
  return { refreshed: true, created: false, sorry, tab, authuser: bound.authuser };
}

/**
 * 在冷却窗口内反复提取 cookie：刷新后 Google 需要几秒才把新的
 * __Secure-1PSIDTS 写回 cookie store，所以这里以 3 秒为间隔轮询，
 * 直到拿到完整登录 cookie 或窗口耗尽。整个过程中不做任何导航。
 */
async function collectWithinCooldown(maxMs, storeId, scopedUrl) {
  const budget = Math.max(3000, maxMs || 60000);
  const deadline = Date.now() + budget;
  let map = await readGoogleCookies(storeId, scopedUrl);
  if (isLoggedIn(map)) return { map, waited_ms: 0, exhausted: false };
  while (Date.now() < deadline) {
    await sleep(3000);
    map = await readGoogleCookies(storeId, scopedUrl);   // 每次调用都会刷新 SW 的空闲计时
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

    const bound = await getBoundGeminiTab();
    const map = bound.ok ? await readGoogleCookies(bound.storeId, bound.url) : new Map();
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
 *   1) 距上次刷新 ≥ 120s → 刷新一次页面，然后在 120s 冷却窗口里轮询提取；
 *   2) 距上次刷新 < 60s → 完全不导航，直接提取当前 cookie；
 *   3) 提取不到登录 cookie 时，直接跳过入池（不反复刷新）。
 */
async function sync(reason, _internal) {
  const c = await cfg();
  if (!c.enabled && reason !== 'manual' && reason !== 'host') {
    return { ok: false, detail: '扩展已停用' };
  }
  const bound = await getBoundGeminiTab();
  if (!bound.ok) {
    const detail = bound.detail || '未绑定 Gemini 页面，进入休眠';
    await setCfg({ lastSyncAt: Date.now(), lastSyncOk: false, lastSyncDetail: detail });
    return { ok: false, detail };
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
    let map = await readGoogleCookies(bound.storeId, bound.url);
    let logged = isLoggedIn(map);

    if (!logged || since >= cd) {
      // 冷却已过（或当前读不到登录 cookie）：允许刷新一次
      // 一律不 force：手动触发也受 120 秒冷却约束，防止连点造成连环刷新
      const r = await refreshGeminiTab(false);
      refreshed = !!r.refreshed;   // 冷却未过时 r.refreshed=false，只提取不导航
      if (refreshed) {
        const got = await collectWithinCooldown(cd, bound.storeId, bound.url);
        map = got.map;
        logged = isLoggedIn(map);
        if (got.exhausted && !logged) log('冷却窗口内仍未取到登录 cookie');
      } else {
        map = await readGoogleCookies(bound.storeId, bound.url);
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
      map = await readGoogleCookies(bound.storeId, bound.url);
    }

    const current = await getBoundGeminiTab();
    if (!current.ok || current.tab.id !== bound.tab.id || current.authuser !== bound.authuser) {
      return { ok: false, detail: '绑定页面已改变，本轮未入池；请重新绑定当前页' };
    }
    const identity = await probePageIdentity(bound.tab.id);
    if (!bound.email) {
      const r = { ok: false, detail: '绑定记录缺少账号邮箱（旧版绑定），请重新点「绑定当前页」' };
      await setCfg({ lastSyncAt: Date.now(), lastSyncOk: false, lastSyncDetail: r.detail });
      return r;
    }
    if (!identity.email) {
      const r = { ok: false, detail: '读不到绑定页的账号（' + (identity.error || '页面未暴露邮箱') + '），本轮未入池' };
      await setCfg({ lastSyncAt: Date.now(), lastSyncOk: false, lastSyncDetail: r.detail });
      return r;
    }
    if (!sameEmail(identity.email, bound.email)) {
      const r = { ok: false, detail: '绑定页账号已变为 ' + identity.email + '，本轮未入池；请重新绑定' };
      await setCfg({ lastSyncAt: Date.now(), lastSyncOk: false, lastSyncDetail: r.detail });
      return r;
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
      // Google 多账号共用 cookie jar，真正决定账号的是绑定页的 URL 槽位。
      account: String(bound.authuser),
      page_authuser: bound.authuser,
      bound_tab_id: bound.tab.id,
      bound_url: bound.url,
      // Server re-checks that /u/<page_authuser>/app really is this account.
      account_email: bound.email,
      profile,
      reason: reason || 'auto',
      url: bound.url,
      ts,
      cookie: header,
      sapisidhash,
      refreshed,
      rotated: Date.now() - (Number(c.lastRotateAt) || 0) < 120000,
      logged_in: true,
      user_agent: navigator.userAgent,
      summary: cookieSummary(map),
      client: CLIENT,
    };

    // ── 推送目标（两种模式，2026-09-17 新增远程模式）────────────────────
    //
    // controller 模式（默认）：推给本机/同网的 Chromium 控制器
    //   http://<controller>:9280/cookie-sync（无鉴权，靠内网限制）
    //
    // service 模式（远程服务器）：直推 gemini-web2api 服务端
    //   http://<server>:8083/api/browser/ingest（Bearer API key 鉴权）
    //   用于「浏览器在你自己电脑、服务在远端服务器」的场景 —— 这是
    //   远程接入的**唯一可行路径**：控制器只监听服务器内网，外部够不着。
    const mode = (c.pushMode === 'service') ? 'service' : 'controller';
    const base = (c.controller || '').replace(/\/+$/, '');
    const url = mode === 'service' ? (base + '/api/browser/ingest') : (base + '/cookie-sync');
    let r;
    try {
      const headers = { 'Content-Type': 'application/json' };
      if (mode === 'service') {
        headers['X-GW2A-Ext-Mode'] = mode;
        if (c.token) headers['Authorization'] = 'Bearer ' + c.token;
      } else if (c.token) {
        headers['X-GW2A-Ext-Mode'] = mode;
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
        // 服务端会在入库后用默认模型校验，把结论带回面板
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
    log('sync', r);
    return r;
  } finally {
    if (own) releaseBusy();
  }
}

// ---------------------------------------------------------------- status + alarms

/** Send a lightweight liveness report. This never refreshes or navigates the
 * bound page; it only reads the bound store and tells the server the mode. */
async function reportExtensionStatus(detail) {
  const c = await cfg();
  if (!c.enabled) return { ok: false, detail: '扩展已停用' };
  // Liveness must not depend on a binding: an unbound extension is still
  // alive, it just has nothing to capture. Report it as logged_in=false.
  const bound = await getBoundGeminiTab();
  const map = bound.ok ? await readGoogleCookies(bound.storeId, bound.url) : new Map();
  const mode = c.pushMode === 'service' ? 'service' : 'controller';
  const base = (c.controller || '').replace(/\/+$/, '');
  const url = mode === 'service'
    ? base + '/api/browser/extension-status'
    : base + '/extension-status';
  const headers = { 'Content-Type': 'application/json', 'X-GW2A-Ext-Mode': mode };
  if (mode === 'service' && c.token) headers.Authorization = 'Bearer ' + c.token;
  if (mode === 'controller' && c.token) headers['X-GW2A-Token'] = c.token;
  const payload = {
    profile: await profileName(),
    push_mode: mode,
    logged_in: bound.ok && isLoggedIn(map),
    cookie_count: map.size,
    account: bound.ok ? String(bound.authuser) : '',
    page_authuser: bound.ok ? bound.authuser : null,
    bound_tab_id: bound.ok ? bound.tab.id : 0,
    bound_url: bound.ok ? bound.url : '',
    account_email: bound.ok ? (bound.email || '') : '',
    detail: bound.ok ? (detail || '扩展状态心跳') : ((detail || '扩展状态心跳') + '（' + (bound.detail || '未绑定') + '）'),
    client: CLIENT,
  };
  try {
    const resp = await fetch(url, { method: 'POST', headers, body: JSON.stringify(payload) });
    const body = await resp.text();
    if (!resp.ok) return { ok: false, http: resp.status, detail: body.slice(0, 180) };
    return { ok: true, http: resp.status };
  } catch (e) {
    return { ok: false, detail: String((e && e.message) || e) };
  }
}

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
    delayInMinutes: 0.5,
    periodInMinutes: 5,
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
    } else if (a.name === ALARM_STATUS) {
      const r = await reportExtensionStatus('定时状态心跳');
      if (!r.ok && r.detail && !/扩展已停用/.test(r.detail)) log('status heartbeat failed', r);
    }
  } catch (e) { log('alarm error', e); }
});

// ---------------------------------------------------------------- lifecycle

chrome.runtime.onInstalled.addListener(async (d) => {
  log('installed', d.reason);
  await ensureAlarms();
  // Installation/reload must never navigate an existing Gemini page. The
  // user explicitly starts capture with bind/read/sync from the popup.
});

chrome.runtime.onStartup.addListener(async () => {
  await ensureAlarms();
  try { await keepalive(false); } catch (e) {}
  try { await reportExtensionStatus('扩展启动状态'); } catch (e) {}
});

// gemini 页面加载完成时顺手同步（登录后立刻入池）。
// 注意：sync 内部有 60 秒冷却保护，这里不会引起连环刷新。
chrome.tabs.onUpdated.addListener(async (tabId, info, tab) => {
  if (info.status !== 'complete') return;
  const bound = await getBoundGeminiTab();
  if (!bound.ok || !tab || tabId !== bound.tab.id) return;
  if (!isGeminiUrl(tab.url || '')) return;
  if (BUSY) return;                       // 自己刷新引起的加载，直接忽略
  try {
    const c = await cfg();
    if (!c.enabled) return;
    const map = await readGoogleCookies(bound.storeId, bound.url);
    if (isLoggedIn(map)) await sync('page-load');
  } catch (e) { log('onUpdated sync error', e); }
});

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    try {
      switch (msg && msg.type) {
        case 'status': {
          const c = await cfg();
          const bound = await getBoundGeminiTab();
          const map = bound.ok ? await readGoogleCookies(bound.storeId, bound.url) : new Map();
          const last = Number(c.lastRefreshAt) || 0;
          const cd = cooldownMs(c);
          sendResponse({
            ok: true, config: c, logged_in: isLoggedIn(map),
            cookie_count: map.size, summary: cookieSummary(map),
            cooldown_left_ms: Math.max(0, cd - (Date.now() - last)),
            bound: bound.ok ? {
              tab_id: bound.tab.id,
              url: bound.url,
              authuser: bound.authuser,
              store_id: bound.storeId || '',
              email: bound.email || '',
              detail: '已绑定 ' + (bound.email ? bound.email + ' · ' : '') + authuserLabel(bound.authuser),
            } : { tab_id: 0, url: '', authuser: null, detail: bound.detail },
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
        case 'bindCurrentPage':
          sendResponse(await bindCurrentPage(msg));
          break;
        case 'unbindCurrentPage':
          sendResponse(await unbindCurrentPage());
          break;
        case 'readPage':
          sendResponse(await readCurrentPage());
          break;
        case 'sync':
          sendResponse(await sync('manual'));
          break;
        case 'syncStatus':
          sendResponse(await reportExtensionStatus('用户手动同步状态'));
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
          sendResponse({ ok: true, pong: true, client: CLIENT });
          break;
        case 'keepalive':
          sendResponse(await keepalive(true));
          break;
        case 'sync':
          sendResponse(await sync('host'));
          break;
        case 'status': {
          const c = await cfg();
          const bound = await getBoundGeminiTab();
          const map = bound.ok ? await readGoogleCookies(bound.storeId, bound.url) : new Map();
          const cd = cooldownMs(c);
          sendResponse({
            ok: true, config: c, logged_in: isLoggedIn(map), cookie_count: map.size,
            cooldown_left_ms: Math.max(0, cd - (Date.now() - (Number(c.lastRefreshAt) || 0))),
            bound: bound.ok ? { tab_id: bound.tab.id, url: bound.url, authuser: bound.authuser,
              store_id: bound.storeId || '' } : { detail: bound.detail },
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
