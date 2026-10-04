const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const script = fs.readFileSync(path.join(__dirname, '../static/app.js'), 'utf8');

function setup(preloaded = false) {
  const listeners = {}, requests = [];
  const oldPage = {}, newPage = {};
  let currentPage = oldPage, replacements = 0;
  const button = { disabled: false, textContent: '刷新额度' };
  const newButton = { disabled: true, textContent: '新渠道正在刷新' };
  const panel = { replaceWith() { ++replacements; } };
  const channelPanel = {
    getAttribute: () => 'codex',
    querySelector: (s) => s.includes('button') ? button : panel,
  };
  const form = {
    action: 'https://portal.test/quota/refresh',
    matches: (s) => s === '[data-quota-refresh-form]',
    querySelector: (s) => s === "button[type='submit']" ? button : null,
    closest: (s) => s === '[data-upstream-channel-page]' ? oldPage : s === '[data-upstream-channel-panel]' ? (preloaded ? channelPanel : null) : panel,
  };
  const document = {
    querySelector(s) {
      if (s === '[data-upstream-channel-page]') return currentPage;
      if (s === '[data-upstream-quotas]') return panel;
      if (s.includes('button')) return newButton;
      return null;
    },
    querySelectorAll: () => [], addEventListener: (name, callback) => { listeners[name] = callback; },
    importNode: (node) => node,
  };
  const window = { location: { href: 'https://portal.test/admin/upstreams?channel=codex' }, addEventListener() {} };
  function fetch(url, options) { return new Promise((resolve, reject) => requests.push({ url, options, resolve, reject })); }
  vm.runInNewContext(script, {
    document, window, fetch, URL, URLSearchParams,
    FormData: class { *[Symbol.iterator]() { yield ['channel', 'codex']; } },
    DOMParser: class { parseFromString() {
      const next = { getAttribute: () => 'false' };
      return { querySelector: () => next, querySelectorAll: () => [
        { getAttribute: () => 'antigravity', querySelector() { throw new Error('foreign channel quota read'); } },
        { getAttribute: () => 'codex', querySelector: () => next },
      ] };
    } },
  });
  return {
    requests, button, newButton, replacements: () => replacements,
    submit() { listeners.submit({ target: form, preventDefault() {} }); },
    switchChannel() { if (!preloaded) currentPage = newPage; window.location.href = 'https://portal.test/admin/upstreams?channel=antigravity'; },
    async respond(index, ok = true) {
      requests[index].resolve({ ok, status: ok ? 200 : 502, text: () => Promise.resolve('quota') });
      await new Promise(setImmediate);
    },
  };
}

test('an old channel quota result cannot replace the new channel panel', async () => {
  const f = setup();
  f.submit();
  await f.respond(0);
  assert.equal(f.requests[1].url, 'https://portal.test/admin/upstreams?channel=codex');
  f.switchChannel();
  await f.respond(1);
  assert.equal(f.replacements(), 0);
});

test('preloaded channels refresh the original hidden panel, not the visible channel', async () => {
  const f = setup(true);
  f.submit();
  f.switchChannel();
  await f.respond(0);
  assert.equal(f.requests[1].url, 'https://portal.test/admin/upstreams?channel=codex');
  assert.equal(f.requests[1].options.headers['X-Upstream-Channel-Only'], 'true');
  await f.respond(1);
  assert.equal(f.replacements(), 1);
  assert.equal(f.newButton.textContent, '新渠道正在刷新');
});

test('preloaded refresh errors restore only the original channel button', async () => {
  const f = setup(true);
  f.submit();
  f.switchChannel();
  await f.respond(0, false);
  assert.equal(f.button.disabled, false);
  assert.equal(f.button.textContent, '刷新失败，请重试');
  assert.equal(f.newButton.disabled, true);
});

test('an old quota refresh failure cannot reset the new channel refresh button', async () => {
  const f = setup();
  f.submit();
  f.switchChannel();
  await f.respond(0, false);
  assert.equal(f.newButton.disabled, true);
  assert.equal(f.newButton.textContent, '新渠道正在刷新');
});
