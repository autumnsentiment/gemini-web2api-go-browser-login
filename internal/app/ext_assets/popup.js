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
    chrome.runtime.sendMessage(Object.assign({ type }, extra || {}), (r) => {
      if (chrome.runtime.lastError) resolve({ ok: false, detail: chrome.runtime.lastError.message });
      else resolve(r || { ok: false, detail: 'no response' });
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
