'use strict';

const $ = (id) => document.getElementById(id);

function fmtTs(ms) {
  if (!ms) return '—';
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function send(type, extra) {
  return new Promise((resolve) => {
    const once = (attempt) => {
      chrome.runtime.sendMessage(Object.assign({ type }, extra || {}), (r) => {
        const err = chrome.runtime.lastError && chrome.runtime.lastError.message;
        if (err) {
          // MV3 已知问题：SW 冷启动时第一条消息可能报
          // "Receiving end does not exist"。等 400ms 重试一次，
          // 第二次再失败才把错误交给 UI（否则弹窗一打开就「读取失败」）。
          if (attempt === 0 && /Receiving end does not exist|message port closed/i.test(err)) {
            setTimeout(() => once(1), 400);
            return;
          }
          resolve({ ok: false, detail: err });
        } else {
          resolve(r || { ok: false, detail: 'no response' });
        }
      });
    };
    once(0);
  });
}

// 哪些字段是用户正在编辑的（有焦点或被改过但没保存）。
// 周期渲染只刷新状态区，绝不能覆盖这些字段 —— 否则用户填一半就被重置
// （2026-09-17 线上事故：渲染循环把「推送模式」每次都打回存储值，根本配不了）。
let editingSince = 0;
const EDITABLE_IDS = ['profile', 'pushMode', 'controller', 'token',
  'keepalivePeriodMin', 'autoSyncMin', 'statusPeriodMin', 'refreshCooldownSec'];

for (const id of EDITABLE_IDS) {
  const el = document.getElementById(id);
  if (!el) continue;
  el.addEventListener('focus', () => { editingSince = Date.now(); });
  el.addEventListener('input', () => { editingSince = Date.now(); });
  el.addEventListener('change', () => { editingSince = Date.now(); });
}

// 用户最后一次交互（编辑/保存）后 90 秒内不回填表单字段；
// 超过后认为用户已离开，恢复周期同步。
function formFrozen() {
  return editingSince > 0 && (Date.now() - editingSince) < 90000;
}

async function render() {
  const r = await send('status');
  if (!r.ok) { $('login').textContent = '读取失败: ' + r.detail; return; }
  const c = r.config || {};

  // 状态区始终刷新（只写 textContent，不碰表单）
  $('login').textContent = r.logged_in ? '已登录 ✓' : '未登录 / 缺会话 cookie';
  $('dot').className = 'dot ' + (r.logged_in ? 'ok' : 'bad');
  $('ckcount').textContent = r.cookie_count + ' 个 google cookie';

  $('lastka').textContent = fmtTs(c.lastKeepaliveAt) + (c.lastKeepaliveDetail ? ' · ' + c.lastKeepaliveDetail : '');
  $('lastsync').textContent = fmtTs(c.lastSyncAt);
  $('syncdetail').textContent = (c.lastSyncOk ? '✓ ' : '✗ ') + (c.lastSyncDetail || '—');
  $('laststatsync').textContent = fmtTs(c.lastStatusSyncAt);
  $('statsyncdetail').textContent = (c.lastStatusSyncOk ? '✓ ' : '✗ ') + (c.lastStatusSyncDetail || '—');

  // 固定抓取页状态：只写状态文本，不重建列表（列表有自己的刷新时机，
  // 否则 15 秒一次的周期渲染会把用户正在看的列表滚动位置打乱）。
  const pinId = Number(c.pinnedTabId) || 0;
  $('pinned').textContent = pinId
    ? ('已固定 · ' + (c.pinnedAccount ? ('账号槽位 ' + c.pinnedAccount) : '默认账号'))
    : '未固定（自动选择）';
  // 多账号固定到 /u/N/ 时入池标识会自动带 -uN 后缀（不同账号要占池子里不同行）
  const base = (c.profile || 'browser1');
  const slot = String(c.pinnedAccount || '').trim();
  $('effprofile').textContent = slot
    ? (base.endsWith('-u' + slot) ? base : (base + '-u' + slot))
    : base;

  // 表单字段只在「用户不在编辑中」时回填，避免覆盖输入
  if (formFrozen()) return;

  $('profile').value = c.profile || '';
  $('pushMode').value = c.pushMode === 'controller' ? 'controller' : 'service';
  $('controller').value = c.controller || '';
  $('token').value = c.token || '';
  $('enabled').checked = !!c.enabled;
  $('autoKeepalive').checked = !!c.autoKeepalive;
  $('keepalivePeriodMin').value = c.keepalivePeriodMin || 10;
  $('autoSyncMin').value = c.autoSyncMin || 30;
  $('statusPeriodMin').value = c.statusPeriodMin || 5;
  $('refreshCooldownSec').value = c.refreshCooldownSec || 120;
}

async function save() {
  await send('setConfig', {
    patch: {
      profile: $('profile').value.trim(),
      pushMode: $('pushMode').value === 'controller' ? 'controller' : 'service',
      controller: $('controller').value.trim() || 'http://127.0.0.1:9280',
      token: $('token').value.trim(),
      enabled: $('enabled').checked,
      autoKeepalive: $('autoKeepalive').checked,
      keepalivePeriodMin: Number($('keepalivePeriodMin').value) || 10,
      autoSyncMin: Number($('autoSyncMin').value) || 30,
      statusPeriodMin: Number($('statusPeriodMin').value) || 5,
      refreshCooldownSec: Number($('refreshCooldownSec').value) || 120,
    },
  });
  editingSince = 0; // 保存成功后解除冻结，下一次 render 回填已保存的值
  await render();
}

$('btnSave').addEventListener('click', save);
$('btnKa').addEventListener('click', async () => {
  $('btnKa').disabled = true; $('btnKa').textContent = '保活中…';
  await send('keepalive');
  $('btnKa').disabled = false; $('btnKa').textContent = '立即保活';
  await render();
});
$('btnSync').addEventListener('click', async () => {
  $('btnSync').disabled = true; $('btnSync').textContent = '入池中…';
  await send('sync');
  $('btnSync').disabled = false; $('btnSync').textContent = '立即入池';
  await render();
});
$('btnStatusSync').addEventListener('click', async () => {
  $('btnStatusSync').disabled = true; $('btnStatusSync').textContent = '同步中…';
  await send('statusSync');
  $('btnStatusSync').disabled = false; $('btnStatusSync').textContent = '同步状态';
  await render();
});
// 枚举本机所有已登录的 Google 账号槽位，逐个入池（第二个账号落 -u1 行）。
$('btnSyncAll').addEventListener('click', async () => {
  $('btnSyncAll').disabled = true; $('btnSyncAll').textContent = '抓取中…';
  const r = await send('syncAll');
  $('btnSyncAll').disabled = false; $('btnSyncAll').textContent = '抓取全部账号';
  await render();
  await renderTabs();
});

// ---- 固定抓取页 / 读取当前页 -------------------------------------------

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

async function renderTabs() {
  const box = $('tabs');
  box.innerHTML = '<div class="empty">读取中…</div>';
  const r = await send('listTabs');
  const tabs = (r && r.tabs) || [];
  const cfgNow = await send('status');
  const pinId = Number((cfgNow.config || {}).pinnedTabId) || 0;
  if (!tabs.length) {
    box.innerHTML = '<div class="empty">没有打开 Gemini 页面。先在本机浏览器打开 gemini.google.com 并登录。</div>';
    return;
  }
  box.innerHTML = tabs.map((t) => {
    const pinned = t.tabId === pinId;
    const state = t.logged_in
      ? '<span class="pill ok">已登录</span>'
      : '<span class="pill bad">未登录</span>';
    const pinPill = pinned ? '<span class="pill pin">已固定</span>' : '';
    return `
      <div class="tab ${pinned ? 'pinned' : ''}">
        <div class="info">
          <div class="t1">
            <b>${esc(t.account_label)}</b>
            ${state}${pinPill}${t.active ? '<span class="pill">当前窗口</span>' : ''}
          </div>
          <div class="t2" title="${esc(t.url)}">${esc(t.title || t.url)} · ${t.cookie_count} 个 cookie</div>
        </div>
        <button class="pick" data-read="${t.tabId}">读取此页</button>
        <button class="pick" data-pin="${t.tabId}">${pinned ? '重新固定' : '固定'}</button>
      </div>`;
  }).join('');
}

$('tabs').addEventListener('click', async (ev) => {
  const btn = ev.target.closest('button');
  if (!btn) return;
  const readId = btn.getAttribute('data-read');
  const pinId = btn.getAttribute('data-pin');
  const old = btn.textContent;
  btn.disabled = true;
  try {
    if (readId) {
      btn.textContent = '读取中…';
      const r = await send('readPage', { tabId: Number(readId) });
      btn.textContent = r && r.ok ? '已入池 ✓' : '失败';
      setTimeout(() => { btn.textContent = old; btn.disabled = false; }, 1600);
    } else if (pinId) {
      btn.textContent = '固定中…';
      const r = await send('pinTab', { tabId: Number(pinId) });
      btn.textContent = r && r.ok ? '已固定 ✓' : '失败';
      setTimeout(async () => { await renderTabs(); await render(); }, 900);
    }
  } finally {
    if (readId) { /* 上面已恢复 */ } else { setTimeout(() => { btn.disabled = false; }, 900); }
  }
});

$('btnRefreshTabs').addEventListener('click', renderTabs);
$('btnUnpin').addEventListener('click', async () => {
  $('btnUnpin').disabled = true;
  await send('pinTab', { tabId: 0 });
  await renderTabs();
  await render();
  $('btnUnpin').disabled = false;
});

render();
renderTabs();
// 状态区刷新周期。15 秒足够（Cookie 数/登录态变化很慢），也更少打扰。
// 渲染函数自身有表单冻结保护，不会覆盖用户正在编辑的字段。
setInterval(render, 15000);
