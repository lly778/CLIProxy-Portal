const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const script = fs.readFileSync(path.join(__dirname, '../static/upstream-channels.js'), 'utf8');

function setup(initial = 'codex') {
  const events = {}, windowEvents = {}, history = [];
  const channels = ['codex', 'antigravity', 'gemini'];
  const panels = channels.map(channel => ({
    channel, hidden: channel !== initial,
    fields: { alias: '', keepOriginal: true, reasoning: '', preset: '' },
    getAttribute: key => key === 'data-channel' ? channel : null,
  }));
  const location = { href: 'https://portal.test/admin/upstreams?channel=' + initial, origin: 'https://portal.test' };
  const select = {
    value: initial, options: channels.map(text => ({ text })),
    get selectedIndex() { return channels.indexOf(this.value); },
    addEventListener: (name, fn) => { events[name] = fn; },
  };
  const button = { removed: false, remove() { this.removed = true; } };
  const form = {
    action: 'https://portal.test/admin/upstreams',
    querySelector: s => s.startsWith('select') ? select : button,
    addEventListener: (name, fn) => { events[name] = fn; },
  };
  const status = { textContent: '' };
  let channel = initial;
  const page = {
    getAttribute: () => channel, setAttribute: (_, value) => { channel = value; },
    querySelector: s => s === '.upstream-channel-form' ? form : status,
    querySelectorAll: () => panels,
  };
  // Switching must work offline, without fetch, DOMParser, or confirmation.
  vm.runInNewContext(script, {
    document: { querySelector: () => page }, URL,
    fetch() { throw new Error('switch made a network request'); },
    window: {
      location, confirm() { throw new Error('switch discarded edits'); },
      history: {
        pushState: (_, __, url) => { history.push(url); location.href = url; },
        replaceState: (_, __, url) => { location.href = url; },
      },
      addEventListener: (name, fn) => { windowEvents[name] = fn; },
    },
  });
  return {
    panels, page, select, button, status, history, location,
    choose(value) { select.value = value; events.change(); },
    submit(value) { select.value = value; let prevented = false; events.submit({ preventDefault() { prevented = true; } }); return prevented; },
    back(url) { location.href = url; windowEvents.popstate(); },
  };
}

test('switching synchronously toggles preloaded cards without network or replacement', () => {
  const f = setup(), original = f.panels.slice();
  assert.equal(f.button.removed, true);
  for (const channel of ['antigravity', 'gemini', 'codex']) {
    f.choose(channel);
    assert.equal(f.page.getAttribute('data-channel'), channel);
    assert.deepEqual(f.panels.filter(p => !p.hidden).map(p => p.channel), [channel]);
    assert.deepEqual(f.panels, original);
    assert.match(f.status.textContent, new RegExp(channel));
  }
  assert.equal(f.history.length, 3);
});

test('aliases, checkboxes, reasoning and preset names survive round-trip switches', () => {
  const f = setup();
  const edits = { alias: 'public-model', keepOriginal: false, reasoning: 'low', preset: '日常' };
  Object.assign(f.panels[0].fields, edits);
  f.choose('antigravity');
  f.panels[1].fields.alias = 'another-model';
  f.choose('codex');
  assert.deepEqual(f.panels[0].fields, edits);
  f.choose('antigravity');
  assert.equal(f.panels[1].fields.alias, 'another-model');
});

test('history toggles existing panels without adding entries or losing edits', () => {
  const f = setup('antigravity');
  f.panels[1].fields.alias = 'edited';
  f.choose('gemini');
  f.back('https://portal.test/admin/upstreams?channel=antigravity');
  assert.equal(f.page.getAttribute('data-channel'), 'antigravity');
  assert.equal(f.panels[1].fields.alias, 'edited');
  f.back('https://portal.test/admin/upstreams');
  assert.equal(f.page.getAttribute('data-channel'), 'codex');
  assert.equal(f.history.length, 1);
});

test('unknown channels cannot hide all cards and history restores a valid URL', () => {
  const f = setup();
  f.choose('unknown');
  assert.equal(f.select.value, 'codex');
  assert.equal(f.panels[0].hidden, false);
  f.back('https://portal.test/admin/upstreams?channel=unknown');
  assert.equal(f.location.href, 'https://portal.test/admin/upstreams?channel=codex');
  assert.equal(f.history.length, 0);
});

test('keyboard form submission switches locally and same-channel selection adds no history', () => {
  const f = setup();
  assert.equal(f.submit('antigravity'), true);
  f.choose('antigravity');
  assert.equal(f.history.length, 1);
  assert.equal(f.page.getAttribute('data-channel'), 'antigravity');
});
