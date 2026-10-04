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
  let captured = null, pending = null, disconnected = false, asyncScroll = false, emulateSnap = false;
  const scrollCalls = [];
  const classes = new Set(); viewport.classList = {
    add: c => classes.add(c), contains: c => classes.has(c),
    remove(c) {
      classes.delete(c);
      if (emulateSnap && c === 'is-dragging' && !classes.has('is-settling')) {
        viewport.scrollLeft = Math.round(viewport.scrollLeft / viewport.clientWidth) * viewport.clientWidth;
      }
    },
  };
  viewport.setPointerCapture = id => { captured = id; }; viewport.hasPointerCapture = id => captured === id; viewport.releasePointerCapture = () => { captured = null; };
  viewport.scrollTo = ({ left, behavior }) => {
    scrollCalls.push({ from: viewport.scrollLeft, left, behavior });
    if (asyncScroll && behavior === 'smooth') pending = left;
    else { pending = null; viewport.scrollLeft = left; viewport.emit('scroll'); }
  };
  track.querySelectorAll = () => slides;
  root.querySelectorAll = () => buttons;
  root.querySelector = s => ({ '[data-quota-viewport]': viewport, '[data-quota-track]': track, '[data-quota-controls]': controls, '[data-quota-previous]': previous, '[data-quota-next]': next, '[data-quota-carousel-status]': status }[s]);
  const window = node(); window.matchMedia = () => ({ matches: false });
  window.getComputedStyle = slide => ({ display: layouts[slide.getAttribute('data-quota-provider')] === (viewport.clientWidth <= 680 ? 'wide' : 'narrow') ? 'none' : 'flex' });
  window.ResizeObserver = class { observe() {} disconnect() { disconnected = true; } };
  vm.runInNewContext(script, { window, document: { querySelectorAll: () => [] } });
  window.QuotaCarousel.init(root, preferred);
  return { window, root, viewport, slides, buttons, controls, previous, next, classes, status, scrollCalls,
    selected: () => window.QuotaCarousel.selected(root), disconnected: () => disconnected,
    asyncScroll() { asyncScroll = true; },
    emulateSnap() { emulateSnap = true; },
    animationClock() {
      let time = 0, nextID = 0;
      const frames = new Map();
      window.performance = { now: () => time };
      window.requestAnimationFrame = callback => { const id = ++nextID; frames.set(id, callback); return id; };
      window.cancelAnimationFrame = id => frames.delete(id);
      return {
        time(value) { time = value; },
        tick(value) { time = value; const pending = [...frames.values()]; frames.clear(); pending.forEach(callback => callback(time)); },
        pending: () => frames.size,
      };
    },
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

test('release animates from the dragged position and restores snapping only after landing', () => {
  const css = fs.readFileSync(path.join(__dirname, '../static/style.css'), 'utf8');
  assert.match(css, /\.quota-carousel-viewport\.is-settling\s*\{\s*scroll-snap-type:\s*none;\s*\}/);
  for (const [start, dx, target] of [[0, -220, 600], [0, -390, 600], [1, 220, 0], [1, 390, 0], [0, -30, 0], [1, 30, 600]]) {
    const f = setup(undefined, ['Codex', 'Antigravity'][start]);
    f.asyncScroll(); f.emulateSnap();
    f.pointer('pointerdown', 500); f.pointer('pointermove', 500 + dx);
    const released = f.viewport.scrollLeft;
    f.pointer('pointerup', 500 + dx);
    assert.deepEqual(f.scrollCalls.at(-1), { from: released, left: target, behavior: 'smooth' });
    assert.equal(f.viewport.scrollLeft, released, 'release must not resnap before animation');
    assert.equal(f.classes.has('is-dragging'), false);
    assert.equal(f.classes.has('is-settling'), true);
    f.scroll((released + target) / 2);
    assert.equal(f.classes.has('is-settling'), true);
    f.finishScroll();
    assert.equal(f.classes.has('is-settling'), false);
    assert.equal(f.viewport.scrollLeft, target);
  }
});

test('cancel, wheel interruption, repeated dragging and destroy clear settling state', () => {
  const f = setup(); f.asyncScroll();
  f.pointer('pointerdown', 500); f.pointer('pointermove', 280); f.pointer('pointercancel', 280);
  assert.equal(f.viewport.scrollLeft, 0); assert.equal(f.classes.has('is-settling'), false);
  f.pointer('pointerdown', 500); f.pointer('pointermove', 280); f.pointer('pointerup', 280);
  f.scroll(300);
  f.pointer('pointerdown', 280);
  assert.deepEqual(f.scrollCalls.at(-1), { from: 300, left: 300, behavior: 'auto' });
  f.pointer('pointermove', 140); f.pointer('pointerup', 140); f.finishScroll();
  assert.equal(f.classes.has('is-settling'), false);
  f.pointer('pointerdown', 100); f.pointer('pointermove', 250); f.pointer('pointerup', 250);
  f.viewport.emit('wheel'); assert.equal(f.classes.has('is-settling'), false);
  f.pointer('pointerdown', 100); f.pointer('pointermove', 250); f.pointer('pointerup', 250);
  f.window.QuotaCarousel.destroy(f.root);
  assert.equal(f.classes.has('is-settling'), false); assert.equal(f.classes.has('is-dragging'), false);
});

test('release animation carries drag velocity immediately and lands monotonically without native ease-in', () => {
  for (const [start, dx, target] of [[0, -200, 600], [1, 200, 0], [0, -390, 600], [1, 390, 0], [0, -30, 0], [1, 30, 600]]) {
    const f = setup(undefined, ['Codex', 'Antigravity'][start]);
    const clock = f.animationClock(); f.emulateSnap();
    f.pointer('pointerdown', 500);
    clock.time(32); f.pointer('pointermove', 500 + dx / 2);
    clock.time(64); f.pointer('pointermove', 500 + dx);
    const released = f.viewport.scrollLeft, calls = f.scrollCalls.length;
    f.pointer('pointerup', 500 + dx);
    assert.equal(f.viewport.scrollLeft, released);
    assert.equal(f.scrollCalls.length, calls, 'release must not start native smooth scrolling');
    clock.tick(65);
    if (Math.abs(dx) >= 72) {
      const speed = Math.abs(dx) / 64;
      assert.ok(Math.abs(f.viewport.scrollLeft - released) >= speed * .97, 'first frame must carry the release velocity');
      assert.ok(Math.abs(f.viewport.scrollLeft - released) <= speed * 1.03);
    }
    let previous = f.viewport.scrollLeft;
    for (let t = 80; t <= 464; t += 16) {
      clock.tick(t);
      const position = f.viewport.scrollLeft;
      assert.ok(position >= Math.min(released, target) && position <= Math.max(released, target), 'no overshoot or jump to origin');
      assert.ok(Math.abs(target - position) <= Math.abs(target - previous), 'release motion must stay monotonic');
      previous = position;
    }
    assert.equal(f.viewport.scrollLeft, target);
    assert.equal(clock.pending(), 0);
    assert.equal(f.classes.has('is-settling'), false);
  }
});

test('release frames stop on new input, resize, cancellation and destroy; reduced motion stays instant', () => {
  const interrupts = [
    f => f.viewport.emit('wheel'),
    f => f.window.emit('resize'),
    f => f.next.emit('click'),
    f => f.pointer('pointerdown', 300, 0, { pointerType: 'touch' }),
    f => f.pointer('pointerdown', 300),
    f => f.window.QuotaCarousel.destroy(f.root),
  ];
  for (const interrupt of interrupts) {
    const f = setup(), clock = f.animationClock();
    f.pointer('pointerdown', 500); clock.time(64); f.pointer('pointermove', 300); f.pointer('pointerup', 300);
    clock.tick(80);
    assert.equal(clock.pending(), 1);
    interrupt(f);
    assert.equal(clock.pending(), 0);
    const interrupted = f.viewport.scrollLeft;
    clock.tick(500);
    assert.equal(f.viewport.scrollLeft, interrupted, 'old frame must not overwrite new input');
  }
  const f = setup(), clock = f.animationClock();
  f.pointer('pointerdown', 500); clock.time(64); f.pointer('pointermove', 300); f.pointer('pointercancel', 300);
  assert.equal(clock.pending(), 0); assert.equal(f.viewport.scrollLeft, 0);
  f.window.QuotaCarousel.destroy(f.root);
  f.window.matchMedia = () => ({ matches: true });
  f.window.QuotaCarousel.init(f.root);
  f.pointer('pointerdown', 500); clock.time(128); f.pointer('pointermove', 300); f.pointer('pointerup', 300);
  assert.equal(clock.pending(), 0); assert.equal(f.viewport.scrollLeft, 600); assert.equal(f.classes.has('is-settling'), false);
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

test('text selection does not start a carousel drag or suppress its native click', () => {
  const f = setup();
  const textTarget = { closest: selector => selector.includes('[data-quota-text]') ? {} : null };
  f.pointer('pointerdown', 300, 0, { target: textTarget });
  const move = f.pointer('pointermove', 100, 0, { target: textTarget });
  f.pointer('pointerup', 100, 0, { target: textTarget });
  assert.equal(move.prevented, undefined);
  assert.equal(f.classes.has('is-dragging'), false);
  assert.equal(f.viewport.scrollLeft, 0);
  assert.equal(f.selected(), 'Codex');
  assert.equal(f.viewport.emit('click', { target: textTarget }).prevented, undefined);
  f.pointer('pointerdown', 300); f.pointer('pointermove', 100); f.pointer('pointerup', 100);
  assert.equal(f.selected(), 'Antigravity', 'blank space must still drag');
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
