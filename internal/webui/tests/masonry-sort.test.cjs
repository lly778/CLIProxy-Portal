const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const script = fs.readFileSync(path.join(__dirname, '../static/masonry-sort.js'), 'utf8');

function classes(initial = []) {
  const values = new Set(initial);
  return { add: value => values.add(value), remove: value => values.delete(value), contains: value => values.has(value) };
}
function setup({ saved = null, ids = ['a', 'b', 'c'], scope = 'aliases:codex', touch = true, saveMode = 'ok', csrf = 'test-csrf', siblingStatus = false } = {}) {
  const documentEvents = {}, windowEvents = {}, writes = [], requests = [];
  const cards = ids.map((id, index) => {
    const events = {}, attributes = { 'aria-label': '移动模型 ' + id };
    const handle = {
      events, hidden: true, focused: false, captured: null,
      getAttribute: name => attributes[name], setAttribute: (name, value) => { attributes[name] = value; },
      addEventListener: (name, fn) => { events[name] = fn; },
      setPointerCapture(pointerId) { this.captured = pointerId; },
      releasePointerCapture() { this.captured = null; },
      focus() { this.focused = true; },
    };
    return {
      id, handle, style: {}, classList: classes(), fields: { alias: 'public-' + id, name: 'alias_' + index, keepOriginal: true },
      getAttribute: () => id, querySelector: () => handle,
      getBoundingClientRect: () => ({ left: index * 110, right: index * 110 + 100, top: 0, bottom: 100 }),
    };
  });
  const status = { textContent: '' };
  const attributes = { 'data-masonry-key': scope, 'data-masonry-order': saved, 'data-masonry-csrf': csrf };
  const list = {
    classList: classes(['is-masonry']), getAttribute: name => attributes[name],
    querySelector: () => siblingStatus ? null : status, parentElement: { querySelector: () => status },
  };
  const body = { classList: classes() };
  const window = {
    PointerEvent: touch ? function () {} : undefined,
    get localStorage() { throw new Error('shared layout must never use local storage'); },
    fetch(url, options) {
      const body = new URLSearchParams(options.body);
      writes.push([body.get('scope'), body.get('order')]);
      const request = { url, options, body };
      requests.push(request);
      if (saveMode === 'defer') return new Promise((resolve, reject) => { request.resolve = resolve; request.reject = reject; });
      if (saveMode === 'network') return Promise.reject(new Error('offline'));
      return Promise.resolve({
        ok: saveMode !== 'http',
        json: async () => {
          if (saveMode === 'html') throw new Error('not JSON');
          return { ok: saveMode !== 'rejected' };
        },
      });
    },
    addEventListener: (name, fn) => { windowEvents[name] = fn; },
  };
  vm.runInNewContext(script, { window, URLSearchParams, document: { body, addEventListener: (name, fn) => { documentEvents[name] = fn; } } });
  let visual = cards.slice();
  const sorter = window.PortalMasonrySort.init({ list, cards, applyOrder(order) { visual = Array.from(order); } });
  function event(x = 10, y = 10, overrides = {}) {
    return Object.assign({ clientX: x, clientY: y, pointerId: 1, button: 0, isPrimary: true, prevented: false, preventDefault() { this.prevented = true; } }, overrides);
  }
  function send(card, type, x = 10, y = 10, overrides = {}) {
    const ev = event(x, y, overrides);
    card.handle.events[type](ev);
    return ev;
  }
  return {
    cards, body, writes, requests, status, sorter, windowEvents, documentEvents,
    flushSaves: () => new Promise(resolve => setImmediate(resolve)),
    order: () => visual.map(card => card.id), send, event,
    key(card, key) { return send(card, 'keydown', 0, 0, { key }); },
  };
}

test('handle drop saves shared order without mutating form associations', async () => {
  const f = setup(), [a, b] = f.cards;
  const originals = f.cards.map(card => ({ ...card.fields }));
  assert.ok(f.cards.every(card => !card.handle.hidden));
  f.send(a, 'pointerdown');
  assert.ok(f.body.classList.contains('is-masonry-sorting'));
  f.send(a, 'pointermove', 150, 50);
  assert.equal(a.style.transform, 'translate(140px,40px)');
  assert.ok(b.classList.contains('is-masonry-drop-target'));
  assert.deepEqual(f.order(), ['a', 'b', 'c'], 'only drop commits');
  f.send(a, 'pointerup', 150, 50);
  assert.deepEqual(f.order(), ['b', 'a', 'c']);
  assert.deepEqual(f.cards.map(card => card.id), ['a', 'b', 'c'], 'DOM/model order stays intact');
  assert.deepEqual(f.cards.map(card => card.fields), originals);
  assert.equal(a.style.transform, '');
  assert.equal(a.handle.captured, null);
  assert.equal(a.handle.focused, true);
  assert.equal(f.body.classList.contains('is-masonry-sorting'), false);
  assert.equal(b.classList.contains('is-masonry-drop-target'), false);
  assert.match(f.status.textContent, /正在保存/);
  await f.flushSaves();
  assert.deepEqual(JSON.parse(f.writes[0][1]), ['b', 'a', 'c']);
  assert.match(f.status.textContent, /共享排列已保存/);
  const request = f.requests[0];
  assert.equal(request.url, '/admin/upstreams/layout');
  assert.equal(request.options.method, 'POST');
  assert.equal(request.options.credentials, 'same-origin');
  assert.equal(request.options.redirect, 'error');
  assert.equal(request.options.keepalive, true);
  assert.equal(request.body.get('csrf_token'), 'test-csrf');
});

test('small clicks, secondary buttons, non-primary pointers and dropping outside cards never save', () => {
  const f = setup(), a = f.cards[0];
  f.send(a, 'pointerdown', 10, 10, { button: 2 });
  assert.equal(a.handle.captured, null);
  f.send(a, 'pointerdown', 10, 10, { isPrimary: false });
  assert.equal(a.handle.captured, null);
  f.send(a, 'pointerdown');
  f.send(a, 'pointerup', 12, 12);
  f.send(a, 'pointerdown');
  f.send(a, 'pointermove', 500, 500);
  f.send(a, 'pointerup', 500, 500);
  assert.deepEqual(f.order(), ['a', 'b', 'c']);
  assert.equal(f.writes.length, 0);
  assert.equal(f.body.classList.contains('is-masonry-sorting'), false);
});

test('cancel, capture loss, Escape, blur, resize, scroll and channel switches clean drag state without saving', () => {
  for (const reason of ['pointercancel', 'lostpointercapture', 'Escape', 'blur', 'resize', 'scroll', 'switch']) {
    const f = setup(), a = f.cards[0];
    f.send(a, 'pointerdown');
    f.send(a, 'pointermove', 150, 50);
    if (reason === 'Escape') f.documentEvents.keydown(f.event(0, 0, { key: reason }));
    else if (reason === 'switch') f.sorter.cancel();
    else if (reason.startsWith('pointer') || reason === 'lostpointercapture') f.send(a, reason);
    else f.windowEvents[reason]();
    assert.deepEqual(f.order(), ['a', 'b', 'c'], reason);
    assert.equal(f.writes.length, 0, reason);
    assert.equal(a.style.transform, '', reason);
    assert.equal(f.body.classList.contains('is-masonry-sorting'), false, reason);
  }
});

test('pointer identity and touch handles are supported without starting a second drag', () => {
  const f = setup(), [a, b] = f.cards;
  f.send(a, 'pointerdown', 10, 10, { pointerType: 'touch' });
  f.send(b, 'pointerdown', 150, 50, { pointerId: 2 });
  assert.equal(b.handle.captured, null);
  f.send(a, 'pointerup', 150, 50, { pointerId: 2 });
  f.send(a, 'pointercancel', 150, 50, { pointerId: 2 });
  assert.equal(f.writes.length, 0);
  f.send(a, 'pointerup', 150, 50);
  assert.deepEqual(f.order(), ['b', 'a', 'c']);
});

test('keyboard ordering persists, respects boundaries and leaves field values intact', async () => {
  const f = setup(), a = f.cards[0];
  assert.equal(f.key(a, 'ArrowLeft').prevented, true);
  assert.equal(f.writes.length, 0);
  f.key(a, 'End');
  assert.deepEqual(f.order(), ['b', 'c', 'a']);
  f.key(a, 'ArrowUp');
  assert.deepEqual(f.order(), ['b', 'a', 'c']);
  f.key(a, 'Home');
  assert.deepEqual(f.order(), ['a', 'b', 'c']);
  assert.equal(a.fields.alias, 'public-a');
  assert.match(a.handle.getAttribute('aria-label'), /移动模型 a，第 1 张/);
  await f.flushSaves();
});

test('saved order restores surviving IDs once and appends new cards without stale IDs', () => {
  const f = setup({ saved: JSON.stringify(['c', 'missing', 'c', 'a']) });
  assert.deepEqual(f.order(), ['c', 'a', 'b']);
  assert.equal(f.writes.length, 0);
  assert.deepEqual(f.cards.map(card => card.id), ['a', 'b', 'c']);
  assert.equal(setup({ saved: '{bad' }).order().join(','), 'a,b,c');
});

test('server layout scopes are separate and never read or write browser storage', async () => {
  for (const scope of ['presets', 'aliases:codex', 'aliases:antigravity']) {
    const f = setup({ scope });
    f.key(f.cards[0], 'End');
    await f.flushSaves();
    assert.equal(f.writes[0][0], scope);
  }
});

test('failed, expired, offline or malformed server replies never claim a shared save', async () => {
  for (const saveMode of ['http', 'network', 'html', 'rejected']) {
    const f = setup({ saveMode, siblingStatus: true });
    f.key(f.cards[0], 'End');
    await f.flushSaves();
    assert.match(f.status.textContent, /保存失败/, saveMode);
    assert.doesNotMatch(f.status.textContent, /已保存/, saveMode);
    assert.deepEqual(f.order(), ['b', 'c', 'a']);
  }
  const missingCSRF = setup({ csrf: '' });
  missingCSRF.key(missingCSRF.cards[0], 'End');
  await missingCSRF.flushSaves();
  assert.equal(missingCSRF.requests.length, 0);
  assert.match(missingCSRF.status.textContent, /未保存/);
});

test('rapid reorder saves are serialized and only the latest pending order is sent', async () => {
  const f = setup({ saveMode: 'defer' }), a = f.cards[0];
  f.key(a, 'End');
  await f.flushSaves();
  assert.equal(f.requests.length, 1);
  f.key(a, 'ArrowUp');
  f.key(a, 'Home');
  await f.flushSaves();
  assert.equal(f.requests.length, 1, 'no parallel writes to the same layout');
  f.requests[0].resolve({ ok: true, json: async () => ({ ok: true }) });
  await f.flushSaves();
  assert.equal(f.requests.length, 2);
  assert.deepEqual(JSON.parse(f.writes[1][1]), ['a', 'b', 'c']);
  assert.match(f.status.textContent, /正在保存/);
  f.requests[1].resolve({ ok: true, json: async () => ({ ok: true }) });
  await f.flushSaves();
  assert.match(f.status.textContent, /已保存/);
});

test('a failed save does not prevent a newer queued layout from saving', async () => {
  const f = setup({ saveMode: 'defer' }), a = f.cards[0];
  f.key(a, 'End');
  await f.flushSaves();
  f.key(a, 'Home');
  f.requests[0].reject(new Error('offline'));
  await f.flushSaves();
  assert.equal(f.requests.length, 2);
  f.requests[1].resolve({ ok: true, json: async () => ({ ok: true }) });
  await f.flushSaves();
  assert.match(f.status.textContent, /已保存/);
});

test('single cards and unsupported pointer browsers keep hidden handles', () => {
  assert.equal(setup({ ids: ['a'] }).cards[0].handle.hidden, true);
  assert.ok(setup({ touch: false }).cards.every(card => card.handle.hidden));
});
