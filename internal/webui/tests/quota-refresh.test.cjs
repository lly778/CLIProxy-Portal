const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const script = fs.readFileSync(path.join(__dirname, '../static/app.js'), 'utf8');

function setup() {
  const listeners = {}, requests = [], carouselEvents = [];
  let replacements = 0;
  const button = { disabled: false, textContent: '刷新额度' };
  function pool() {
    return {
      querySelector: () => button,
      getAttribute: () => 'false',
      replaceWith(next) { active = next; replacements++; },
    };
  }
  let active = pool();
  const form = {
    action: 'https://portal.test/quota/refresh',
    matches: selector => selector === '[data-quota-refresh-form]',
    querySelector: selector => selector === "button[type='submit']" ? button : null,
    closest: selector => selector === '[data-quota-pool]' ? active : null,
  };
  const window = {
    location: { href: 'https://portal.test/dashboard' },
    addEventListener() {},
    QuotaCarousel: {
      selected: () => 'Antigravity',
      destroy: () => carouselEvents.push('destroy'),
      init: (_, selected) => carouselEvents.push(selected),
    },
  };
  const document = {
    querySelector: () => null, querySelectorAll: () => [],
    addEventListener: (name, fn) => { listeners[name] = fn; },
    contains: node => node === active, importNode: node => node,
  };
  vm.runInNewContext(script, {
    window, document, URL, URLSearchParams,
    fetch: (url, options) => new Promise(resolve => requests.push({ url, options, resolve })),
    FormData: class { *[Symbol.iterator]() { yield ['next', '/dashboard']; } },
    DOMParser: class { parseFromString() { return { querySelector: () => pool() }; } },
  });
  return {
    requests, button, carouselEvents,
    replacements: () => replacements,
    submit() { listeners.submit({ target: form, preventDefault() {} }); },
    navigate() { window.location.href = 'https://portal.test/status'; },
    async respond(index, ok = true) {
      requests[index].resolve({ ok, status: ok ? 200 : 502, text: async () => 'quota' });
      await new Promise(setImmediate);
    },
  };
}

test('dashboard quota refresh preserves the selected carousel provider', async () => {
  const state = setup();
  state.submit();
  await state.respond(0);
  assert.equal(state.requests[1].url, 'https://portal.test/dashboard');
  await state.respond(1);
  assert.equal(state.replacements(), 1);
  assert.deepEqual(state.carouselEvents, ['destroy', 'Antigravity']);
});

test('a quota response cannot replace content after leaving its page', async () => {
  const state = setup();
  state.submit();
  await state.respond(0);
  state.navigate();
  await state.respond(1);
  assert.equal(state.replacements(), 0);
});

test('a failed quota refresh restores the current page button', async () => {
  const state = setup();
  state.submit();
  await state.respond(0, false);
  assert.equal(state.button.disabled, false);
  assert.equal(state.button.textContent, '刷新失败，请重试');
});
