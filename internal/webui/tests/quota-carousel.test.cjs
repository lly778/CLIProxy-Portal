const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const script = fs.readFileSync(path.join(__dirname, '../static/quota-carousel.js'), 'utf8');

function setup(names = ['Codex', 'Antigravity', 'Gemini'], preferred, layouts = {}) {
  function node() {
    const listeners = new Map(), attrs = new Map(), classes = new Set();
    return {
      attrs, listeners, style: {}, hidden: false, children: [], textContent: '',
      classList: { add: c => classes.add(c), toggle(c, active) { if (active) classes.add(c); else classes.delete(c); }, contains: c => classes.has(c) },
      appendChild(child) { this.children.push(child); },
      focus() { this.focused = true; },
      addEventListener(name, fn) { if (!listeners.has(name)) listeners.set(name, []); listeners.get(name).push(fn); },
      removeEventListener(name, fn) { listeners.set(name, (listeners.get(name) || []).filter(value => value !== fn)); },
      emit(name, options = {}) { const event = { target: this, preventDefault() { this.prevented = true; }, stopPropagation() { this.stopped = true; }, ...options }; (listeners.get(name) || []).forEach(fn => fn(event)); return event; },
      getAttribute: key => attrs.get(key), setAttribute: (key, value) => attrs.set(key, value),
      closest: () => null,
      querySelector(selector) { return this.children.find(child => '.' + child.className === selector) || null; },
    };
  }
  const viewport = node(), track = node(), root = node(), controls = node(), previous = node(), next = node(), status = node();
  const slides = names.map((name, i) => {
    const slide = node(); slide.attrs.set('data-quota-provider', name); slide.offsetHeight = 300 + i * 150;
    slide.attrs.set('data-quota-channel', name.split(' · ')[0]);
    if (layouts[name]) slide.attrs.set('data-quota-layout', layouts[name]);
    return slide;
  });
  const buttons = names.map(name => { const button = node(); button.attrs.set('data-quota-select', name); return button; });
  viewport.clientWidth = 600; viewport.scrollWidth = names.length * 600; viewport.scrollLeft = 0;
  let captured = null, pending = null, disconnected = false, asyncScroll = false;
  const classes = new Set(); viewport.classList = { add: c => classes.add(c), remove: c => classes.delete(c) };
  viewport.setPointerCapture = id => { captured = id; }; viewport.hasPointerCapture = id => captured === id; viewport.releasePointerCapture = () => { captured = null; };
  viewport.scrollTo = ({ left, behavior }) => { if (asyncScroll && behavior === 'smooth') pending = left; else { viewport.scrollLeft = left; viewport.emit('scroll'); } };
  track.querySelectorAll = () => slides;
  root.querySelectorAll = () => buttons;
  root.querySelector = s => ({ '[data-quota-viewport]': viewport, '[data-quota-track]': track, '[data-quota-controls]': controls, '[data-quota-previous]': previous, '[data-quota-next]': next, '[data-quota-carousel-status]': status }[s]);
  const window = node(); window.matchMedia = () => ({ matches: false });
  window.getComputedStyle = slide => ({ display: layouts[slide.getAttribute('data-quota-provider')] === (viewport.clientWidth <= 680 ? 'wide' : 'narrow') ? 'none' : 'flex' });
  window.ResizeObserver = class { observe() {} disconnect() { disconnected = true; } };
  vm.runInNewContext(script, { window, document: { querySelectorAll: () => [] } });
  window.QuotaCarousel.init(root, preferred);
  return { window, root, viewport, slides, buttons, controls, previous, next, classes, status,
    selected: () => window.QuotaCarousel.selected(root), disconnected: () => disconnected,
    asyncScroll() { asyncScroll = true; },
    finishScroll() { viewport.scrollLeft = pending; viewport.emit('scroll'); },
    scroll(left) { viewport.scrollLeft = left; viewport.emit('scroll'); },
    pointer(name, x, y = 0, extra = {}) { return viewport.emit(name, { clientX: x, clientY: y, pointerId: 1, pointerType: 'mouse', button: 0, ...extra }); },
  };
}

test('only the selected channel is interactive and viewport height follows its card', () => {
  const f = setup();
  assert.equal(f.selected(), 'Codex'); assert.equal(f.viewport.style.height, '300px'); assert.equal(f.previous.disabled, true);
  f.buttons[1].emit('click');
  assert.equal(f.selected(), 'Antigravity'); assert.equal(f.viewport.style.height, '450px');
  assert.deepEqual(f.slides.map(s => s.inert), [true, false, true]);
  assert.equal(f.buttons[1].getAttribute('aria-pressed'), 'true');
  f.next.emit('click'); assert.equal(f.selected(), 'Gemini'); assert.equal(f.next.disabled, true);
});

test('mouse dragging changes one channel, cancels safely, and suppresses the drag click', () => {
  const f = setup();
  f.pointer('pointerdown', 200); f.pointer('pointermove', 80); f.pointer('pointerup', 80);
  assert.equal(f.selected(), 'Antigravity'); assert.equal(f.classes.has('is-dragging'), false);
  assert.equal(f.viewport.emit('click').prevented, true);
  f.pointer('pointerdown', 200); f.pointer('pointermove', 80); f.pointer('pointercancel', 80);
  assert.equal(f.selected(), 'Antigravity');
  f.pointer('pointerdown', 80); f.pointer('pointermove', 220); f.pointer('pointerup', 220);
  assert.equal(f.selected(), 'Codex');
});

test('boundary arrows hide and reappear after clicks, native scroll, keyboard and resize', () => {
  const css = fs.readFileSync(path.join(__dirname, '../static/style.css'), 'utf8');
  assert.match(css, /\.quota-carousel-arrow\[hidden\]\s*\{\s*display:\s*none;\s*\}/);
  const f = setup();
  assert.equal(f.previous.hidden, true); assert.equal(f.next.hidden, false);
  f.next.emit('click');
  assert.equal(f.previous.hidden, false); assert.equal(f.next.hidden, false);
  f.next.emit('click');
  assert.equal(f.previous.hidden, false); assert.equal(f.next.hidden, true);
  f.scroll(600);
  assert.equal(f.previous.hidden, false); assert.equal(f.next.hidden, false);
  f.viewport.emit('keydown', { key: 'Home' });
  assert.equal(f.previous.hidden, true); assert.equal(f.next.hidden, false);
  f.viewport.emit('keydown', { key: 'End' });
  f.viewport.clientWidth = 350; f.window.emit('resize');
  assert.equal(f.previous.hidden, false); assert.equal(f.next.hidden, true);
  const restored = setup(undefined, 'Gemini');
  assert.equal(restored.previous.hidden, false); assert.equal(restored.next.hidden, true);
  const single = setup(['Codex']);
  assert.equal(single.previous.hidden, true); assert.equal(single.next.hidden, true);
});

test('narrow three-card carousel becomes two desktop cards and preserves the selected model family', () => {
  const names = ['Codex', 'Antigravity', 'Antigravity · Claude / GPT', 'Antigravity · Gemini'];
  const layouts = { Antigravity: 'wide', 'Antigravity · Claude / GPT': 'narrow', 'Antigravity · Gemini': 'narrow' };
  const f = setup(names, undefined, layouts);
  assert.equal(f.selected(), 'Codex');
  f.next.emit('click'); assert.equal(f.selected(), 'Antigravity · Claude / GPT');
  f.next.emit('click'); assert.equal(f.selected(), 'Antigravity · Gemini');
  assert.equal(f.next.hidden, true);
  assert.deepEqual(f.slides.map(s => s.inert), [true, true, true, false]);
  f.viewport.clientWidth = 1000; f.viewport.scrollWidth = 2000; f.window.emit('resize');
  assert.equal(f.selected(), 'Antigravity'); assert.equal(f.next.hidden, true);
  f.viewport.clientWidth = 600; f.viewport.scrollWidth = 1800; f.window.emit('resize');
  assert.equal(f.selected(), 'Antigravity · Gemini');
  assert.equal(f.next.hidden, true);
  f.pointer('pointerdown', 80); f.pointer('pointermove', 220); f.pointer('pointerup', 220);
  assert.equal(f.selected(), 'Antigravity · Claude / GPT');
  const restored = setup(names, 'Antigravity · Gemini', layouts);
  assert.equal(restored.selected(), 'Antigravity · Gemini');
  assert.equal(restored.next.hidden, true);
});

test('touch and vertical movement keep native page scroll; interactive controls do not start drags', () => {
  const f = setup();
  assert.equal(f.pointer('pointerdown', 200, 0, { pointerType: 'touch' }).prevented, undefined);
  assert.equal(f.pointer('pointermove', 30, 0, { pointerType: 'touch' }).prevented, undefined);
  f.scroll(600); assert.equal(f.selected(), 'Antigravity');
  f.pointer('pointerdown', 200); assert.equal(f.pointer('pointermove', 190, 100).prevented, undefined); f.pointer('pointerup', 190);
  assert.equal(f.selected(), 'Antigravity');
  f.pointer('pointerdown', 200, 0, { target: { closest: () => ({}) } }); f.pointer('pointermove', 30); f.pointer('pointerup', 30);
  assert.equal(f.selected(), 'Antigravity');
});

test('keyboard and resize preserve the selected provider and single-channel controls stay hidden', () => {
  const f = setup();
  assert.equal(f.viewport.emit('keydown', { key: 'End' }).prevented, true); assert.equal(f.selected(), 'Gemini');
  f.viewport.clientWidth = 350; f.window.emit('resize'); assert.equal(f.viewport.scrollLeft, 700);
  f.viewport.emit('keydown', { key: 'Home' }); assert.equal(f.selected(), 'Codex');
  assert.equal(setup(['Codex']).controls.hidden, true);
});

test('refresh reinitialization restores the selected provider, cleans listeners and tolerates removed providers', () => {
  const f = setup(undefined, 'Antigravity'); assert.equal(f.selected(), 'Antigravity');
  f.window.QuotaCarousel.destroy(f.root); assert.equal(f.disconnected(), true); assert.equal(f.window.listeners.get('resize').length, 0);
  assert.equal(f.root.quotaCarousel, undefined);
  assert.equal(setup(['Codex'], 'Antigravity').selected(), 'Codex');
});

test('smooth programmatic switching does not briefly reset selection to the old channel', () => {
  const f = setup(); f.asyncScroll(); f.next.emit('click');
  f.scroll(40); assert.equal(f.selected(), 'Antigravity');
  f.finishScroll(); assert.equal(f.selected(), 'Antigravity');
});

test('fractional widths clamp the final scroll position to the real browser limit', () => {
  const f = setup(['Codex', 'Antigravity']);
  f.viewport.clientWidth = 511; f.viewport.scrollWidth = 1021;
  f.next.emit('click');
  assert.equal(f.viewport.scrollLeft, 510);
  assert.equal(f.selected(), 'Antigravity');
});
