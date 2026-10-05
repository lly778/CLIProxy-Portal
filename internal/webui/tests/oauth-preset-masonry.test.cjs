const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const script = fs.readFileSync(path.join(__dirname, '../static/upstream-channels.js'), 'utf8');

function setup(heights, initialWidth = 810, initialNarrow = false) {
  let width = initialWidth, observer;
  const frames = [], events = {}, mediaEvents = {}, observed = [];
  const media = { matches: initialNarrow, addEventListener: (name, fn) => { mediaEvents[name] = fn; } };
  const cards = heights.map((height, id) => ({
    id, height, style: {}, getBoundingClientRect() { return { height: this.height }; },
  }));
  const list = {
    style: {}, classes: new Set(), querySelectorAll: () => cards,
    getBoundingClientRect: () => ({ width }),
    classList: { add(name) { list.classes.add(name); } },
  };
  vm.runInNewContext(script, {
    document: { querySelector: selector => selector.includes('.oauth-preset-list') ? list : null },
    window: {
      matchMedia: () => media,
      requestAnimationFrame: fn => frames.push(fn),
      addEventListener: (name, fn) => { events[name] = fn; },
      ResizeObserver: class { constructor(fn) { observer = fn; } observe(target) { observed.push(target); } },
    },
  });
  return {
    list, cards, frames, observed,
    flush() { while (frames.length) frames.shift()(); },
    resize(nextWidth, narrow) { width = nextWidth; media.matches = narrow; events.resize(); mediaEvents.change(); },
    changeHeight(index, height) { cards[index].height = height; observer(); },
  };
}

test('two unequal preset cards occupy left and right columns at the top', () => {
  for (const heights of [[420, 380], [380, 420], [420, 420]]) {
    const f = setup(heights);
    assert.equal(f.cards[0].style.left, '0px');
    assert.equal(f.cards[1].style.left, '410px');
    assert.deepEqual(f.cards.map(card => card.style.top), ['0px', '0px']);
    assert.deepEqual(f.cards.map(card => card.style.width), ['400px', '400px']);
    assert.equal(f.list.style.height, Math.max(...heights) + 'px');
    assert.ok(f.list.classes.has('is-masonry'));
  }
});

test('later cards fill the shorter column without row-height gaps or DOM reordering', () => {
  const f = setup([420, 300, 120, 80]);
  assert.deepEqual(f.cards.map(card => [card.style.left, card.style.top]), [
    ['0px', '0px'], ['410px', '0px'], ['410px', '310px'], ['0px', '430px'],
  ]);
  assert.deepEqual(f.cards.map(card => card.id), [0, 1, 2, 3]);
  assert.equal(f.list.style.height, '510px');
  assert.equal(f.observed.length, 5);
});

test('narrow layouts preserve original order and return to separate wide columns', () => {
  const f = setup([420, 300, 120]);
  f.resize(350, true);
  assert.equal(f.frames.length, 1, 'resize and breakpoint updates should coalesce');
  f.flush();
  assert.deepEqual(f.cards.map(card => [card.style.left, card.style.top, card.style.width]), [
    ['0px', '0px', '350px'], ['0px', '430px', '350px'], ['0px', '740px', '350px'],
  ]);
  assert.equal(f.list.style.height, '860px');
  f.resize(810, false);
  f.flush();
  assert.equal(f.cards[1].style.left, '410px');
  assert.equal(f.cards[1].style.top, '0px');
  assert.equal(f.cards[2].style.top, '310px');
});

test('content height changes reflow following cards and list height', () => {
  const f = setup([420, 300, 120]);
  f.changeHeight(1, 500);
  f.flush();
  assert.equal(f.cards[2].style.left, '0px');
  assert.equal(f.cards[2].style.top, '430px');
  assert.equal(f.list.style.height, '550px');
});

test('empty, single and initially hidden preset lists remain safe', () => {
  const empty = setup([]);
  assert.equal(empty.list.classes.has('is-masonry'), false);
  const single = setup([200]);
  assert.equal(single.cards[0].style.left, '0px');
  assert.equal(single.list.style.height, '200px');
  const hidden = setup([200, 150], 0);
  assert.equal(hidden.list.classes.has('is-masonry'), false);
  hidden.resize(810, false);
  hidden.flush();
  assert.equal(hidden.cards[1].style.left, '410px');
});
