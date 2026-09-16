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

async function render() {
  const r = await send('status');
  if (!r.ok) { $('login').textContent = '读取失败: ' + r.detail; return; }
  const c = r.config || {};

  $('login').textContent = r.logged_in ? '已登录 ✓' : '未登录 / 缺会话 cookie';
  $('dot').className = 'dot ' + (r.logged_in ? 'ok' : 'bad');
  $('ckcount').textContent = r.cookie_count + ' 个 google cookie';

  $('lastka').textContent = fmtTs(c.lastKeepaliveAt) + (c.lastKeepaliveDetail ? ' · ' + c.lastKeepaliveDetail : '');
  $('lastsync').textContent = fmtTs(c.lastSyncAt);
  $('syncdetail').textContent = (c.lastSyncOk ? '✓ ' : '✗ ') + (c.lastSyncDetail || '—');

  $('profile').value = c.profile || '';
  $('controller').value = c.controller || '';
  $('token').value = c.token || '';
  $('enabled').checked = !!c.enabled;
  $('autoKeepalive').checked = !!c.autoKeepalive;
  $('keepalivePeriodMin').value = c.keepalivePeriodMin || 10;
  $('autoSyncMin').value = c.autoSyncMin || 30;
  $('refreshCooldownSec').value = c.refreshCooldownSec || 60;
}

async function save() {
  await send('setConfig', {
    patch: {
      profile: $('profile').value.trim(),
      controller: $('controller').value.trim() || 'http://127.0.0.1:9280',
      token: $('token').value.trim(),
      enabled: $('enabled').checked,
      autoKeepalive: $('autoKeepalive').checked,
      keepalivePeriodMin: Number($('keepalivePeriodMin').value) || 10,
      autoSyncMin: Number($('autoSyncMin').value) || 30,
      refreshCooldownSec: Number($('refreshCooldownSec').value) || 60,
    },
  });
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
setInterval(render, 5000);
