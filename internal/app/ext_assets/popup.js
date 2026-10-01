'use strict';

const $ = (id) => document.getElementById(id);

function fmtTs(ms) {
  if (!ms) return '—';
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function authLabel(n) {
  const v = Number(n) || 0;
  return v > 0 ? '/u/' + v : '默认账号 /u/0';
}

function renderCookieSummary(summary) {
  if (!summary || typeof summary !== 'object') return '—';
  return Object.keys(summary).map((name) => {
    const x = summary[name] || {};
    return `${name}: ${x.len || 0}`;
  }).join(' · ') || '—';
}

function send(type, extra) {
  return new Promise((resolve) => {
    chrome.runtime.sendMessage(Object.assign({ type }, extra || {}), (r) => {
      if (chrome.runtime.lastError) resolve({ ok: false, detail: chrome.runtime.lastError.message });
      else resolve(r || { ok: false, detail: 'no response' });
    });
  });
}

function popupActiveTab() {
  return new Promise((resolve) => {
    chrome.tabs.query({ active: true, currentWindow: true }, (tabs) => {
      if (chrome.runtime.lastError) {
        resolve({ ok: false, detail: chrome.runtime.lastError.message });
        return;
      }
      const tab = tabs && tabs[0];
      if (!tab || tab.id == null) {
        resolve({ ok: false, detail: '扩展弹窗无法读取当前标签页' });
        return;
      }
      resolve({ ok: true, tab });
    });
  });
}

// 哪些字段是用户正在编辑的（有焦点或被改过但没保存）。
// 周期渲染只刷新状态区，绝不能覆盖这些字段 —— 否则用户填一半就被重置
// （2026-09-17 线上事故：渲染循环把「推送模式」每次都打回存储值，根本配不了）。
let editingSince = 0;
const EDITABLE_IDS = ['profile', 'pushMode', 'controller', 'token',
  'keepalivePeriodMin', 'autoSyncMin', 'refreshCooldownSec'];

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

  const b = r.bound || {};
  $('boundState').textContent = b.tab_id ? '已绑定' : (b.detail || '未绑定');
  $('boundAuth').textContent = b.authuser == null ? '—' : authLabel(b.authuser);
  $('boundEmail').textContent = b.email || '—';
  $('boundStore').textContent = b.store_id || '—';
  $('boundUrl').textContent = b.url || '—';
  if (!b.tab_id) $('pageDetail').textContent = b.detail || '请先绑定当前 Gemini 页';

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
  $('refreshCooldownSec').value = c.refreshCooldownSec || 120;
}

async function readPage() {
  const b = $('btnReadPage');
  b.disabled = true; b.textContent = '读取中…';
  try {
    const r = await send('readPage');
    if (!r.ok) {
      $('pageDetail').textContent = r.detail || '读取失败';
      return r;
    }
    const p = r.page || {};
    $('boundState').textContent = '已绑定 · 未刷新';
    $('boundAuth').textContent = p.authuser == null ? '—' : authLabel(p.authuser);
    $('boundUrl').textContent = p.url || '—';
    $('pageEmail').textContent = (p.email || '页面未暴露邮箱') + (p.account_mismatch ? '（与绑定账号不一致）' : '');
    $('pageDetail').textContent = r.detail || '已读取';
    $('cookieFingerprint').textContent = r.cookie_fingerprint ? r.cookie_fingerprint.slice(0, 16) : '—';
    $('cookieSummary').textContent = renderCookieSummary(r.summary);
    return r;
  } finally {
    b.disabled = false; b.textContent = '只读检查';
  }
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
      refreshCooldownSec: Number($('refreshCooldownSec').value) || 120,
    },
  });
  editingSince = 0; // 保存成功后解除冻结，下一次 render 回填已保存的值
  await render();
}

$('btnSave').addEventListener('click', save);
$('btnBind').addEventListener('click', async () => {
  const b = $('btnBind');
  b.disabled = true; b.textContent = '绑定中…';
  try {
    const active = await popupActiveTab();
    if (!active.ok) {
      $('pageDetail').textContent = active.detail || '无法读取当前页面';
      return;
    }
    const r = await send('bindCurrentPage', {
      tabId: active.tab.id,
      tabUrl: active.tab.url || '',
      windowId: active.tab.windowId,
    });
    $('pageDetail').textContent = r.detail || (r.ok ? '已绑定' : '绑定失败');
    if (r.ok) await readPage();
    await render();
  } finally { b.disabled = false; b.textContent = '绑定当前页'; }
});
$('btnReadPage').addEventListener('click', readPage);
$('btnStatusSync').addEventListener('click', async () => {
  const b = $('btnStatusSync');
  b.disabled = true; b.textContent = '同步中…';
  try {
    const r = await send('syncStatus');
    $('pageDetail').textContent = r.ok ? '插件状态已回传服务端' : (r.detail || '状态同步失败');
    await render();
  } finally { b.disabled = false; b.textContent = '同步插件状态'; }
});
$('btnUnbind').addEventListener('click', async () => {
  const b = $('btnUnbind');
  b.disabled = true;
  try { await send('unbindCurrentPage'); await render(); }
  finally { b.disabled = false; }
});
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

render();
// 状态区刷新周期。15 秒足够（Cookie 数/登录态变化很慢），也更少打扰。
// 渲染函数自身有表单冻结保护，不会覆盖用户正在编辑的字段。
setInterval(render, 15000);
