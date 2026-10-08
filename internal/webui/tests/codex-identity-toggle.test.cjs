const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../static/codex-identity-toggle.js'), 'utf8');
function fixture(initial, fetchResult, disabled = false) {
  const handlers = {};
  const control = {checked: initial, disabled, addEventListener(name, action) {handlers[name] = action;}};
  const status = {textContent: '', dataset: {}};
  const form = {action: 'https://portal.test/admin/upstreams/codex-identity', querySelector(selector) {
    return selector.includes('enabled') ? control : status;
  }};
  const calls = [];
  class FormData {
    constructor() {
      this.items = [['channel', 'antigravity'], ['csrf_token', 'test-csrf']];
      if (control.checked) this.items.push(['enabled', 'true']);
    }
    [Symbol.iterator]() {return this.items[Symbol.iterator]();}
  }
  vm.runInNewContext(source, {
    document: {querySelectorAll() {return [form];}}, URLSearchParams, FormData,
    fetch: async (url, options) => {calls.push({url, options}); return fetchResult(options);}
  });
  return {control, status, calls, change: handlers.change};
}

test('toggle saves CSRF and channel without a page reload, including unchecked state', async () => {
  const f = fixture(false, async options => ({ok: true, json: async () => ({enabled: options.body.get('enabled') === 'true'})}));
  f.control.checked = true;
  const pending = f.change();
  assert.equal(f.control.disabled, true);
  await pending;
  assert.equal(f.control.checked, true);
  assert.equal(f.control.disabled, false);
  assert.equal(f.status.textContent, '已保存');
  assert.equal(f.calls[0].options.headers.Accept, 'application/json');
  assert.equal(f.calls[0].options.credentials, 'same-origin');
  assert.equal(f.calls[0].options.body.get('channel'), 'antigravity');
  assert.equal(f.calls[0].options.body.get('csrf_token'), 'test-csrf');
  f.control.checked = false;
  await f.change();
  assert.equal(f.calls[1].options.body.has('enabled'), false);
  assert.equal(f.control.checked, false);
});

test('failed or expired saves restore the last saved value', async () => {
  for (const response of [
    {ok: false, status: 403}, {ok: false, status: 500},
    {ok: true, json: async () => {throw new Error('login page');}}
  ]) {
    const f = fixture(true, async () => response);
    f.control.checked = false;
    await f.change();
    assert.equal(f.control.checked, true);
    assert.equal(f.control.disabled, false);
    assert.equal(f.status.dataset.error, 'true');
  }
});

test('unavailable toggle cannot save', () => {
  const f = fixture(false, async () => {throw new Error('unexpected request');}, true);
  assert.equal(f.change, undefined);
  assert.equal(f.calls.length, 0);
});
