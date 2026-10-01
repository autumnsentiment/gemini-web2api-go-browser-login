'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');

const state = {};
const cookies = [];
const tabs = new Map([
  [1, { id: 1, url: 'https://gemini.google.com/app' }],
  [2, { id: 2, url: 'https://gemini.google.com/u/1/app' }],
  [3, { id: 3, url: 'https://gemini.google.com/u/2/app' }],
  // Account chooser leaves a plain /app URL while the page runs as /u/1.
  [5, { id: 5, url: 'https://gemini.google.com/app' }],
  [6, { id: 6, url: 'https://gemini.google.com/app' }],
]);
// Identity each tab's page reports through WIZ_global_data.
const pages = new Map([
  [1, { email: 'first@example.test', slot: 0, has_token: true }],
  [2, { email: 'second@example.test', slot: 1, has_token: true }],
  [3, { email: 'third@example.test', slot: 2, has_token: true }],
  [5, { email: 'second@example.test', slot: 1, has_token: true }],
  [6, { email: '', slot: null, has_token: false, signed_out: true }],
]);
const requests = [];
const expires = Date.now() / 1000 + 86400;
const makeCookie = (name, value, cookiePath, storeId = '0', domain = '.google.com') =>
  ({ name, value, path: cookiePath, storeId, domain, secure: true, expirationDate: expires });
const domainMatches = (cookie, host) =>
  host === cookie.domain.replace(/^\./, '') || host.endsWith(cookie.domain);
const pathMatches = (cookie, pathname) =>
  pathname === cookie.path || (pathname.startsWith(cookie.path) &&
    (cookie.path.endsWith('/') || pathname.charAt(cookie.path.length) === '/'));

const chrome = {
  storage: { local: {
    async get(defaults) { return { ...defaults, ...state }; },
    async set(patch) { Object.assign(state, patch); },
  } },
  cookies: {
    async getAllCookieStores() { return [{ id: '0', tabIds: [1, 2, 3] }, { id: '1', tabIds: [4] }]; },
    async getAll(query) {
      const list = cookies.filter((ck) => !query.storeId || ck.storeId === query.storeId);
      if (query.domain) {
        // Model a Chromium build that omits the path-specific auth cookie.
        return list.filter((ck) => ck.domain === query.domain && ck.path === '/');
      }
      if (query.url) {
        const u = new URL(query.url);
        return list.filter((ck) => ck.path === '/' &&
          domainMatches(ck, u.hostname) && pathMatches(ck, u.pathname));
      }
      return list;
    },
    async get(query) {
      const u = new URL(query.url);
      return cookies.filter((ck) => ck.name === query.name &&
        (!query.storeId || ck.storeId === query.storeId) &&
        domainMatches(ck, u.hostname) && pathMatches(ck, u.pathname))
        .sort((a, b) => b.path.length - a.path.length)[0] || null;
    },
  },
  tabs: {
    async get(id) {
      if (!tabs.has(id)) throw new Error('closed tab');
      return tabs.get(id);
    },
    onUpdated: { addListener() {} },
  },
  scripting: {
    async executeScript({ target }) {
      return [{ result: Object.assign({ url: tabs.get(target.tabId).url, title: 'Gemini' },
        pages.get(target.tabId)) }];
    },
  },
  alarms: { async clear() {}, create() {}, onAlarm: { addListener() {} } },
  runtime: {
    onInstalled: { addListener() {} },
    onStartup: { addListener() {} },
    onMessage: { addListener() {} },
    onMessageExternal: { addListener() {} },
  },
};

const sandbox = {
  chrome, crypto: webcrypto, TextEncoder, URL, Date, setTimeout,
  navigator: { userAgent: 'test' },
  console: { log() {} },
  async fetch(url, init) {
    requests.push({ url, body: JSON.parse(init.body) });
    return { ok: true, status: 200, async text() { return '{"ok":true}'; } };
  },
};
const code = fs.readFileSync(path.join(__dirname, 'background.js'), 'utf8');
vm.runInNewContext(code + '\nglobalThis.testAPI = { bindCurrentPage, readCurrentPage, ' +
  'readGoogleCookies, sync, setCfg, buildCookieHeader };', sandbox);
const api = sandbox.testAPI;

async function main() {
  for (const name of ['SAPISID', 'SID', '__Secure-1PSID']) {
    cookies.push(makeCookie(name, 'default-' + name, '/'));
    cookies.push(makeCookie(name, 'second-' + name, '/u/1/'));
    cookies.push(makeCookie(name, 'third-' + name, '/u/2/'));
    cookies.push(makeCookie(name, 'wrong-store-' + name, '/u/1/', '1'));
    cookies.push(makeCookie(name, 'wrong-domain-' + name, '/u/1/', '0', '.accounts.google.com'));
  }
  const bound = await api.bindCurrentPage({ tabId: 2 });
  assert.equal(bound.ok, true);
  assert.equal(bound.page.authuser, 1);
  const selected = await api.readGoogleCookies('0', tabs.get(2).url);
  assert.equal(selected.get('SAPISID').value, 'second-SAPISID');
  assert.equal(selected.get('SID').value, 'second-SID');
  assert.ok(!api.buildCookieHeader(selected).header.includes('third-'));
  const first = await api.readGoogleCookies('0', tabs.get(1).url);
  assert.equal(first.get('SAPISID').value, 'default-SAPISID');
  const third = await api.readGoogleCookies('0', tabs.get(3).url);
  assert.equal(third.get('SAPISID').value, 'third-SAPISID');
  const read = await api.readCurrentPage();
  assert.equal(read.page.email, 'second@example.test');
  assert.equal(read.page.account_mismatch, false);
  assert.equal(read.page.authuser, 1);
  assert.equal(read.logged_in, true);

  await api.setCfg({ lastRefreshAt: Date.now(), lastRotateAt: Date.now() });
  const result = await api.sync('manual');
  assert.equal(result.ok, true);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].body.account, '1');
  assert.equal(requests[0].body.page_authuser, 1);
  assert.equal(requests[0].body.bound_url, tabs.get(2).url);
  assert.equal(requests[0].body.account_email, 'second@example.test');
  assert.match(requests[0].body.cookie, /SAPISID=second-SAPISID/);
  assert.doesNotMatch(requests[0].body.cookie, /default-|third-|wrong-/);

  // A shared root cookie is legitimately the same on /app and /u/1/app.
  cookies.splice(0, cookies.length);
  cookies.push(makeCookie('SAPISID', 'shared', '/'));
  cookies.push(makeCookie('SID', 'shared', '/'));
  cookies.push(makeCookie('__Secure-1PSID', 'shared', '/'));
  const sharedDefault = await api.readGoogleCookies('0', tabs.get(1).url);
  const sharedSecond = await api.readGoogleCookies('0', tabs.get(2).url);
  assert.equal(api.buildCookieHeader(sharedDefault).header,
    api.buildCookieHeader(sharedSecond).header);
  const sharedRead = await api.readCurrentPage();
  assert.equal(sharedRead.page.authuser, 1);

  // Plain /app URL showing the second account binds slot 1, not the default.
  const plain = await api.bindCurrentPage({ tabId: 5 });
  assert.equal(plain.ok, true, plain.detail);
  assert.equal(plain.page.authuser, 1);
  assert.equal(state.boundEmail, 'second@example.test');
  await api.setCfg({ lastRefreshAt: Date.now(), lastRotateAt: Date.now() });
  requests.length = 0;
  const plainSync = await api.sync('manual');
  assert.equal(plainSync.ok, true, plainSync.detail);
  assert.equal(requests[0].body.page_authuser, 1);
  assert.equal(requests[0].body.account_email, 'second@example.test');

  // The page switches account in place: ingest must be refused.
  pages.set(5, { email: 'first@example.test', slot: 0, has_token: true });
  requests.length = 0;
  const switched = await api.sync('manual');
  assert.equal(switched.ok, false);
  assert.match(switched.detail, /first@example\.test/);
  assert.equal(requests.length, 0);

  // URL slot and page session disagree: refuse to bind.
  pages.set(3, { email: 'first@example.test', slot: 0, has_token: true });
  const conflict = await api.bindCurrentPage({ tabId: 3 });
  assert.equal(conflict.ok, false);

  // Signed-out page: refuse to bind.
  const anon = await api.bindCurrentPage({ tabId: 6 });
  assert.equal(anon.ok, false);
  console.log('PASS: bound path, store, page identity slot, account switch, signed-out');
}

main().catch((err) => { console.error(err); process.exitCode = 1; });
