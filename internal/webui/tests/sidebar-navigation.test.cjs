const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function setup({ narrow = true, content = 1500, width = 600, saved = '0' } = {}) {
  const source = fs.readFileSync(path.join(__dirname, '../static/app.js'), 'utf8');
  const end = source.indexOf('  document.addEventListener("click"');
  function element(values = {}) {
    return Object.assign({ handlers: {}, addEventListener(name, callback) { this.handlers[name] = callback; } }, values);
  }
  const nav = element({ clientWidth: width - 68, scrollWidth: content, scrollLeft: 0,
    scrollBy({ left }) { this.scrollLeft = Math.max(0, Math.min(this.scrollWidth - this.clientWidth, this.scrollLeft + left)); this.handlers.scroll(); } });
  const controls = element({ clientWidth: width }), previous = element(), next = element();
  const media = element({ matches: narrow }), frames = [];
  const storage = new Map(saved === null ? [] : [['cliproxy-mobile-nav-scroll-left', saved]]);
  const window = element({ matchMedia: query => query.includes('760px') ? media : { matches: true },
    requestAnimationFrame(callback) { frames.push(callback); return frames.length; },
    sessionStorage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
    ResizeObserver: class { constructor(callback) { window.observer = callback; } observe() {} },
  });
  const selectors = { '[data-mobile-nav]': nav, '[data-nav-controls]': controls, '[data-nav-previous]': previous, '[data-nav-next]': next };
  vm.runInNewContext(source.slice(0, end) + '})();', { window, document: { querySelector: key => selectors[key] } });
  const update = () => window.observer();
  const flush = () => { while (frames.length) frames.shift()(); };
  function wheel(values) {
    const event = { deltaX: 0, deltaY: 0, deltaMode: 0, ctrlKey: false, prevented: false,
      preventDefault() { this.prevented = true; }, ...values };
    controls.handlers.wheel(event); nav.handlers.scroll(); flush(); return event;
  }
  return { nav, controls, previous, next, media, window, storage, update, flush, wheel };
}

test('narrow navigation arrows reflect scroll boundaries and preserve saved position', () => {
  const s = setup(); s.flush();
  assert.equal(s.previous.hidden, false); assert.equal(s.previous.disabled, true); assert.equal(s.next.disabled, false);
  s.next.handlers.click(); s.flush(); assert(s.nav.scrollLeft > 0); assert.equal(s.previous.disabled, false);
  assert.equal(s.storage.get('cliproxy-mobile-nav-scroll-left'), String(s.nav.scrollLeft));
  s.nav.scrollLeft = s.nav.scrollWidth - s.nav.clientWidth; s.update(); assert.equal(s.next.disabled, true);
  s.previous.handlers.click(); assert.equal(s.next.disabled, false);
  const restored = setup({ saved: '240' }); restored.flush(); assert.equal(restored.nav.scrollLeft, 240);
});

test('wheel supports both axes and delta units without trapping edge scroll or browser zoom', () => {
  const s = setup(); s.flush();
  assert.equal(s.wheel({ deltaY: 60 }).prevented, true); assert.equal(s.nav.scrollLeft, 60);
  s.wheel({ deltaX: 20, deltaY: 2 }); assert.equal(s.nav.scrollLeft, 80);
  s.wheel({ deltaY: 2, deltaMode: 1 }); assert.equal(s.nav.scrollLeft, 112);
  s.wheel({ deltaY: 1, deltaMode: 2 }); assert.equal(s.nav.scrollLeft, 644);
  assert.equal(s.wheel({ deltaY: 80, ctrlKey: true }).prevented, false); assert.equal(s.nav.scrollLeft, 644);
  s.nav.scrollLeft = s.nav.scrollWidth - s.nav.clientWidth;
  assert.equal(s.wheel({ deltaY: 80 }).prevented, false);
  s.nav.scrollLeft = 0; assert.equal(s.wheel({ deltaY: -80 }).prevented, false);
});

test('wide and non-overflowing navigation hide controls and keep page wheel scrolling', () => {
  const s = setup({ narrow: false });
  assert.equal(s.previous.hidden, true); assert.equal(s.next.hidden, true);
  assert.equal(s.wheel({ deltaY: 80 }).prevented, false);
  s.media.matches = true; s.window.handlers.resize(); assert.equal(s.next.hidden, false);
  s.nav.scrollWidth = 500; s.nav.clientWidth = 600; s.update();
  assert.equal(s.next.hidden, true); assert.equal(s.wheel({ deltaY: 80 }).prevented, false);
});
