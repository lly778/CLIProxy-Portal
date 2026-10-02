const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, '../static/app.js'), 'utf8');

function element(dataset = {}) {
  const attributes = new Map();
  const classes = new Set();
  const listeners = new Map();
  return {
    dataset, attributes, listeners, style: {}, hidden: false, textContent: '',
    classList: { add: (name) => classes.add(name), remove: (name) => classes.delete(name), contains: (name) => classes.has(name) },
    setAttribute(name, value) { attributes.set(name, String(value)); if (name === 'hidden') this.hidden = true; },
    getAttribute(name) { return attributes.get(name) ?? null; },
    removeAttribute(name) { attributes.delete(name); if (name === 'hidden') this.hidden = false; },
    toggleAttribute(name, enabled) { if (enabled) this.setAttribute(name, ''); else this.removeAttribute(name); },
    addEventListener(name, callback, options) { if (!listeners.has(name)) listeners.set(name, []); listeners.get(name).push({ callback, options }); },
    dispatch(name, fields = {}) {
      const event = { cancelable: true, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, ...fields };
      for (const entry of listeners.get(name) ?? []) entry.callback(event);
      return event;
    },
    querySelector() { return null; }, querySelectorAll() { return []; },
    getBoundingClientRect() { return { left: 0, top: 0, width: 80, height: 24 }; },
  };
}

function fixture(count, width, health = false, options = {}) {
  const chart = element();
  const svg = element();
  const plot = element();
  const bars = options.stacked === false ? [
    element({ barX: 496, barY: 118, barWidth: 8, barHeight: 100 }),
  ] : health ? [
    element({ barX: 496, barY: 208, barWidth: 8, barHeight: 10, barSquare: 'true' }),
    element({ barX: 496, barY: 138, barWidth: 8, barHeight: 70, barSquare: 'true' }),
    element({ barX: 496, barY: 118, barWidth: 8, barHeight: 20, barSquare: 'false' }),
  ] : [
    element({ barX: 496, barY: 198, barWidth: 8, barHeight: 20, barSquare: 'true' }),
    element({ barX: 496, barY: 148, barWidth: 8, barHeight: 50, barSquare: 'true' }),
    element({ barX: 496, barY: 118, barWidth: 8, barHeight: 30, barSquare: 'false' }),
  ];
  if (options.thinTop) {
    const top = bars.at(-1), below = bars.at(-2);
    below.dataset.barHeight += top.dataset.barHeight - 0.01;
    below.dataset.barY = 118.01;
    top.dataset.barHeight = 0.01;
  }
  const stack = element(), stackClip = element(), stackTarget = element();
  const stackOutline = element({ barX: 496, barY: 118, barWidth: 8, barHeight: 100 });
  stack.querySelector = (selector) => ({ '[data-trend-bar-clip]': stackClip, '[data-trend-bar-outline]': stackOutline, '[data-trend-bar-stack-target]': stackTarget })[selector] ?? null;
  stack.querySelectorAll = (selector) => selector === '.trend-bar[data-bar-x]' ? bars : [];
  const tooltip = element();
  tooltip.hidden = true;
  tooltip.getBoundingClientRect = () => ({ width: 230, height: 100 });
  const tooltipDate = element(), requestValue = element(), tokenValue = element();
  const healthValues = ['successRate', 'failureRate', 'averageTotal', 'averageUpload', 'averageWait', 'averageResponse'].map((trendValue) => element({ trendValue }));
  const tokenValues = ['inputTokens', 'cacheTokens', 'outputTokens'].map((trendValue) => element({ trendValue }));
  tooltip.querySelector = (selector) => ({ '[data-trend-date]': tooltipDate, '[data-trend-requests]': requestValue, '[data-trend-tokens]': tokenValue })[selector] ?? null;
  tooltip.querySelectorAll = (selector) => selector === '[data-trend-value]' ? (health ? healthValues : tokenValues) : [];
  const points = Array.from({ length: count }, (_, i) => {
    const x = Math.round(100 + i * 800 / Math.max(1, count - 1));
    const point = element({ x, y: 100, tokenY: 160, date: `point-${i}`, requests: '20', tokens: '1000', inputTokens: '200', cacheTokens: '500', outputTokens: '300', successRate: '95%', failureRate: '5%', averageTotal: '4 s', averageUpload: '500 ms', averageWait: '1.5 s', averageResponse: '2 s' });
    point.dots = Array.from({ length: health ? 2 : 1 }, () => {
      const dot = element(); dot.setAttribute('cx', x); return dot;
    });
    point.querySelectorAll = (selector) => selector === '.trend-dot' ? point.dots : [];
    return point;
  });
  const labels = points.map((point, i) => element({ x: point.dataset.x, tick: i }));
  const selection = { svg, '[data-trend-tooltip]': tooltip, '.trend-cursor': element(), '[data-trend-plot]': plot, '[data-trend-clip]': element(), '[data-trend-clip-target]': element() };
  chart.clientWidth = width; chart.clientHeight = 260;
  chart.getBoundingClientRect = () => ({ left: 0, top: 0, width, height: 260 });
  chart.querySelector = (selector) => selection[selector] ?? null;
  chart.querySelectorAll = (selector) => ({ '[data-trend-point]': points, '[data-trend-label]': labels, '.trend-bar[data-bar-x]': bars, '[data-trend-bar-stack]': options.stacked === false ? [] : [stack] })[selector] ?? [];
  chart.contains = (target) => target === svg || target === chart;
  chart.setPointerCapture = () => {};
  chart.hasPointerCapture = () => false;
  const matrix = { a: width / 1000, b: 0, c: 0, d: 1, e: 0, f: 0, inverse() { return { a: 1000 / width, b: 0, c: 0, d: 1, e: 0, f: 0 }; } };
  svg.getScreenCTM = () => matrix;
  svg.getBoundingClientRect = () => ({ width, height: 260 });
  svg.createSVGPoint = () => ({ x: 0, y: 0, matrixTransform(m) { return { x: this.x * m.a + this.y * m.c + m.e, y: this.x * m.b + this.y * m.d + m.f }; } });
  const document = { querySelector: () => null, querySelectorAll: (selector) => selector === '[data-usage-trend], [data-health-trend]' ? (options.chartIndex ? [element(), chart] : [chart]) : [], addEventListener() {} };
  const windowListeners = new Map();
  vm.runInNewContext(script, { document, window: { addEventListener(name, callback) { windowListeners.set(name, callback); } } }, { filename: 'app.js' });
  function resize(nextWidth) {
    width = nextWidth;
    chart.clientWidth = width;
    matrix.a = width / 1000;
    windowListeners.get('resize')();
  }
  function touch(id, x, y = 100) { return { identifier: id, clientX: x * width / 1000, clientY: y, target: svg }; }
  function send(name, touches, changedTouches = []) { return chart.dispatch(name, { touches, changedTouches }); }
  function viewport() {
    const transform = plot.getAttribute('transform');
    if (!transform) return { scale: 1, offset: 0 };
    const values = transform.match(/matrix\(([^)]+)\)/)[1].split(' ').map(Number);
    return { scale: values[0], offset: values[4] };
  }
  function visiblePoints() { const view = viewport(); return points.filter((p) => p.dataset.x * view.scale + view.offset >= 99.999 && p.dataset.x * view.scale + view.offset <= 900.001); }
  function pinch() {
    send('touchstart', [touch(1, 400)]);
    return send('touchstart', [touch(1, 400), touch(2, 600)]);
  }
  return { chart, tooltip, tooltipDate, healthValues, tokenValues, points, bars, stackOutline, stackClip, stackTarget, resize, touch, send, viewport, visiblePoints, pinch };
}

for (const health of [false, true]) {
  for (const width of [400, 1000]) {
    const name = `${health ? 'health' : 'usage'} at ${width}px`;
    test(`${name}: pinch zoom, pan, point limits and cancellation`, () => {
      const f = fixture(60, width, health);
      assert.equal(f.chart.listeners.get('touchmove')[0].options.passive, false);
      assert.ok(f.pinch().defaultPrevented);
      assert.ok(f.send('touchmove', [f.touch(1, 200), f.touch(2, 800)]).defaultPrevented);
      assert.equal(f.viewport().scale, 3);
      assert.ok(f.bars.every((bar) => !bar.getAttribute('d').includes(' A')));
      assert.ok(f.stackOutline.getAttribute('d').includes(' A'));
      if (health) {
        assert.ok(f.bars.slice(0, 2).every((bar) => !bar.getAttribute('d').includes(' A')));
        assert.ok(!f.bars[2].getAttribute('d').includes(' A'));
        assert.equal(f.bars[0].getAttribute('d'), 'M496,208 h8 v10 h-8 Z');
        assert.equal(f.bars[1].getAttribute('d'), 'M496,138 h8 v70 h-8 Z');
      }
      assert.ok(f.visiblePoints().length <= 36);
      assert.ok(f.visiblePoints().every((p) => p.dots.every((dot) => !dot.hidden)));
      f.chart.dispatch('pointermove', { pointerType: 'touch', pointerId: 1, clientX: 0, clientY: 0 });
      f.chart.dispatch('pointercancel', { pointerType: 'touch', pointerId: 1 });
      f.send('touchmove', [f.touch(1, 300), f.touch(2, 900)]);
      assert.equal(f.viewport().offset, -900);
      f.send('touchend', [f.touch(1, 300)], [f.touch(2, 900)]);
      f.send('touchmove', [f.touch(1, 350)]);
      assert.equal(f.viewport().offset, -850);
      f.send('touchend', [], [f.touch(1, 350)]);
      assert.ok(!f.chart.classList.contains('trend-dragging'));
      assert.ok(f.tooltip.hidden);
      f.pinch();
      f.send('touchmove', [f.touch(1, -2000), f.touch(2, 3000)]);
      assert.ok(f.visiblePoints().length >= 12);
      const limit = f.viewport().scale;
      f.send('touchmove', [f.touch(1, -3000), f.touch(2, 4000)]);
      assert.equal(f.viewport().scale, limit);
      f.send('touchmove', [f.touch(1, 400), f.touch(2, 600)]);
      assert.equal(f.viewport().scale, 1);
      assert.equal(f.visiblePoints().length, 60);
      assert.ok(f.points.every((p) => p.dots.every((dot) => dot.hidden)));
      f.send('touchcancel', []);
      const before = f.viewport();
      f.send('touchmove', [f.touch(1, 100), f.touch(2, 900)]);
      assert.deepEqual(f.viewport(), before);
      assert.ok(!f.chart.classList.contains('trend-dragging'));
    });
    test(`${name}: tap values and allow vertical page scroll`, () => {
      const f = fixture(60, width, health);
      assert.ok(!f.send('touchstart', [f.touch(1, 500)]).defaultPrevented);
      f.send('touchend', [], [f.touch(1, 500)]);
      assert.ok(!f.tooltip.hidden);
      assert.match(f.tooltipDate.textContent, /^point-/);
      if (health) assert.deepEqual(f.healthValues.map((v) => v.textContent), ['95%', '5%', '4 s', '500 ms', '1.5 s', '2 s']);
      else assert.deepEqual(f.tokenValues.map((v) => v.textContent), ['200', '500', '300']);
      f.send('touchstart', [f.touch(1, 500)]);
      assert.ok(!f.send('touchmove', [f.touch(1, 501, 150)]).defaultPrevented);
      f.send('touchend', [], [f.touch(1, 501, 150)]);
      assert.equal(f.viewport().scale, 1);
      assert.ok(f.tooltip.hidden);
    });
    test(`${name}: mouse wheel/drag still work`, () => {
      const f = fixture(60, width, health);
      f.chart.dispatch('wheel', { clientX: width / 2, clientY: 100, deltaY: -Math.log(2) / 0.002, deltaMode: 0 });
      assert.equal(f.viewport().scale, 2);
      const offset = f.viewport().offset;
      f.chart.dispatch('pointerdown', { pointerType: 'mouse', button: 0, pointerId: 1, clientX: width / 2, clientY: 100 });
      f.chart.dispatch('pointermove', { pointerType: 'mouse', pointerId: 1, clientX: width / 2 + width / 20, clientY: 100 });
      assert.equal(f.viewport().offset, offset + 50);
      f.chart.dispatch('pointerup', { pointerType: 'mouse', pointerId: 1 });
      assert.ok(!f.chart.classList.contains('trend-dragging'));
    });
    test(`${name}: ignore outside touches and rebase a continuing pinch`, () => {
      const f = fixture(60, width, health);
      const outside = { ...f.touch(3, 500), target: {} };
      f.send('touchstart', [outside]);
      assert.ok(!f.send('touchstart', [f.touch(1, 400), outside]).defaultPrevented);
      assert.ok(!f.send('touchmove', [f.touch(1, 400), outside]).defaultPrevented);
      assert.equal(f.viewport().scale, 1);
      f.send('touchcancel', []);
      f.pinch();
      f.send('touchmove', [f.touch(1, 50), f.touch(2, 950), f.touch(3, 800)]);
      const before = f.viewport();
      // One finger leaves while the others have moved past the plot edges.
      f.send('touchend', [f.touch(1, 50), f.touch(3, 800)], [f.touch(2, 950)]);
      f.send('touchmove', [f.touch(1, 50), f.touch(3, 800)]);
      assert.deepEqual(f.viewport(), before);
      f.send('touchcancel', []);
      assert.ok(!f.chart.classList.contains('trend-dragging'));
    });
  }
}

test('12 or fewer points stay unzoomable but support tap values', () => {
  const f = fixture(12, 400);
  assert.ok(!f.chart.classList.contains('trend-interactive'));
  assert.ok(!f.pinch().defaultPrevented);
  f.send('touchmove', [f.touch(1, 100), f.touch(2, 900)]);
  assert.equal(f.viewport().scale, 1);
  assert.equal(f.points.length, 12);
});

for (const health of [false, true]) {
  test(`${health ? 'health' : 'usage'}: thin top segments retain full-column rounded corners on resize and zoom`, () => {
    const f = fixture(60, 400, health, { thinTop: true });
    const radiusY = () => Number(f.stackOutline.getAttribute('d').match(/ A[^,]+,([^ ]+)/)[1]);
    assert.equal(f.stackClip.id, 'trend-bar-clip-0-0');
    assert.equal(f.stackTarget.getAttribute('clip-path'), 'url(#trend-bar-clip-0-0)');
    assert.ok(Math.abs(radiusY() - 0.8) < 1e-9, 'radius must not depend on the 0.01px top segment');
    assert.ok(f.bars.every((bar) => !bar.getAttribute('d').includes(' A')));
    f.pinch();
    f.send('touchmove', [f.touch(1, 200), f.touch(2, 800)]);
    assert.ok(Math.abs(radiusY() - 2.4) < 1e-9);
    f.resize(1000);
    assert.equal(radiusY(), 4);
    const first = f.bars[0], top = f.bars.at(-1);
    assert.equal(first.dataset.barY + first.dataset.barHeight, 218, 'bottom baseline stays square and fixed');
    assert.equal(top.dataset.barY, 118, 'total top and segment proportions stay fixed');
    assert.equal(top.dataset.barHeight, 0.01);
    const otherChart = fixture(60, 400, !health, { chartIndex: 1 });
    assert.notEqual(otherChart.stackClip.id, f.stackClip.id, 'the two charts must not share clip IDs');
  });
  test(`${health ? 'health' : 'usage'}: legacy total-only bars still have rounded top corners`, () => {
    const f = fixture(60, 400, health, { stacked: false });
    assert.ok(f.bars[0].getAttribute('d').includes(' A'));
    f.resize(1000);
    assert.ok(f.bars[0].getAttribute('d').includes(' A'));
  });
}

test('three Token greens are dark/light/dark with a similar lightness range to the blue timing stages', () => {
  const css = fs.readFileSync(path.join(__dirname, '../static/style.css'), 'utf8');
  function luminance(color) {
    const rgb = color.match(/[\da-f]{2}/gi).map((v) => parseInt(v, 16) / 255).map((v) => v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4);
    return rgb[0] * 0.2126 + rgb[1] * 0.7152 + rgb[2] * 0.0722;
  }
  function color(part) { return css.match(new RegExp(`\\.trend-bar\\.tokens-${part},[^\\n]+fill: (#[\\da-f]{6});`))[1]; }
  for (const [one, two] of [['input', 'cache'], ['cache', 'output']]) {
    const values = [luminance(color(one)), luminance(color(two))].sort((a, b) => b - a);
    assert.ok((values[0] + 0.05) / (values[1] + 0.05) >= 1.8, `${one}/${two} need a clearer dark/light distinction`);
  }
  assert.ok(luminance(color('input')) < luminance(color('cache')));
  assert.ok(luminance(color('cache')) > luminance(color('output')));
  assert.ok(luminance(color('cache')) < 0.6, 'cache must not return to a near-white green');
  for (const [tokenPart, timingPart] of [['input', 'upload'], ['cache', 'wait'], ['output', 'response']]) {
    const timingColor = css.match(new RegExp(`\\.trend-bar\\.duration-${timingPart},[^\\n]+fill: (#[\\da-f]{6});`))[1];
    assert.ok(Math.abs(luminance(color(tokenPart)) - luminance(timingColor)) < 0.1, 'green and blue stages should have similar lightness');
  }
});
