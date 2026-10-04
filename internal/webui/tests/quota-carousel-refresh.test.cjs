const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

test('overview quota refresh preserves the channel and model group selected while polling is in flight', async () => {
  const source = fs.readFileSync(path.join(__dirname, '../static/app.js'), 'utf8');
  const listeners = {}, requests = [], calls = [];
  let selected = 'Codex';
  const replacement = { getAttribute: () => 'false' };
  const root = { replaceWith(next) { assert.equal(next, replacement); calls.push('replace'); } };
  const button = { disabled: false };
  const form = {
    action: '/quota/refresh', matches: s => s === '[data-quota-refresh-form]',
    closest: s => s === '[data-quota-pool]' ? root : null,
    querySelector: s => s.includes('button') ? button : null,
  };
  const document = {
    querySelector: s => s === '[data-quota-pool]' ? root : null,
    querySelectorAll: () => [], importNode: node => node,
    addEventListener: (event, callback) => { listeners[event] = callback; },
  };
  const window = { location: { href: 'https://portal.test/dashboard' }, addEventListener() {}, QuotaCarousel: {
    selected(value) { assert.equal(value, root); return selected; },
    destroy(value) { assert.equal(value, root); calls.push('destroy'); },
    init(value, provider) { assert.equal(value, replacement); calls.push(provider); },
  } };
  vm.runInNewContext(source, { document, window, URLSearchParams,
    FormData: class { *[Symbol.iterator]() {} },
    fetch: (url, options) => new Promise(resolve => requests.push({ url, options, resolve })),
    DOMParser: class { parseFromString() { return { querySelector: () => replacement }; } },
  });
  listeners.submit({ target: form, preventDefault() {} });
  requests[0].resolve({ ok: true }); await new Promise(setImmediate);
  selected = 'Antigravity · Gemini';
  requests[1].resolve({ ok: true, text: () => Promise.resolve('pool') }); await new Promise(setImmediate);
  assert.deepEqual(calls, ['destroy', 'replace', 'Antigravity · Gemini']);
});
