const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const script = fs.readFileSync(path.join(__dirname, '../static/upstream-channels.js'), 'utf8');

function setup(heights, initialWidth = 814, initialViewport = 1400, secondChannel = false) {
  let width = initialWidth, viewport = initialViewport;
  const frames = [], resizeHandlers = [], observers = [], clickHandlers = [];
  const medias = [];
  const cards = heights.map((height, id) => ({
    id, height, style: {}, fields: { alias: 'alias-' + id, keepOriginal: true },
    getBoundingClientRect() { return { height: this.height }; },
  }));
  const list = {
    style: {}, classes: new Set(), querySelectorAll: () => cards,
    getBoundingClientRect: () => ({ width }),
    classList: { add(name) { list.classes.add(name); } },
    addEventListener: (_, fn) => clickHandlers.push(fn),
  };
  const otherCards = [{ height: 250, style: {}, getBoundingClientRect() { return { height: this.height }; } }];
  let otherVisible = false;
  const other = {
    style: {}, querySelectorAll: () => otherCards,
    classList: { add() {} },
    getBoundingClientRect: () => ({ width: otherVisible ? width : 0 }),
    addEventListener() {},
  };
  vm.runInNewContext(script, {
    document: {
      querySelector: () => null,
      querySelectorAll: () => secondChannel ? [list, other] : [list],
    },
    window: {
      matchMedia(query) {
        const maximum = Number(query.match(/\d+/)[0]);
        const media = { get matches() { return viewport <= maximum; }, addEventListener() {} };
        medias.push(media);
        return media;
      },
      requestAnimationFrame: fn => frames.push(fn),
      addEventListener: (_, fn) => resizeHandlers.push(fn),
      ResizeObserver: class { constructor(fn) { observers.push(fn); } observe() {} },
    },
  });
  return {
    list, cards, other, otherCards, frames,
    flush() { while (frames.length) frames.shift()(); },
    resize(nextWidth, nextViewport) { width = nextWidth; viewport = nextViewport; resizeHandlers.forEach(fn => fn()); },
    edit(index, height) { cards[index].height = height; clickHandlers.forEach(fn => fn()); observers[0](); },
    revealOther() { otherVisible = true; observers[1](); },
  };
}

test('alias cards share the shortest-column layout without touching vertical input fields', () => {
  const f = setup([400, 250, 180, 100]);
  assert.deepEqual(f.cards.map(card => [card.style.left, card.style.top]), [
    ['0px', '0px'], ['414px', '0px'], ['414px', '264px'], ['0px', '414px'],
  ]);
  assert.deepEqual(f.cards.map(card => card.style.width), ['400px', '400px', '400px', '400px']);
  assert.equal(f.list.style.height, '514px');
  assert.deepEqual(f.cards.map(card => card.id), [0, 1, 2, 3]);
  assert.deepEqual(f.cards[0].fields, { alias: 'alias-0', keepOriginal: true });
});

test('adding and removing aliases updates card positions and outer height', () => {
  const f = setup([400, 250, 180]);
  f.edit(1, 500);
  assert.equal(f.frames.length, 1, 'button and observer notifications should coalesce');
  f.flush();
  assert.equal(f.cards[2].style.left, '0px');
  assert.equal(f.cards[2].style.top, '414px');
  assert.equal(f.list.style.height, '594px');
  f.edit(1, 250);
  f.flush();
  assert.equal(f.cards[2].style.left, '414px');
  assert.equal(f.cards[2].style.top, '264px');
  assert.equal(f.list.style.height, '444px');
});

test('alias column count follows existing 260px minimum, gaps and narrow ordering', () => {
  const f = setup([400, 250, 180]);
  f.resize(530, 700);
  f.flush();
  assert.equal(f.cards[0].style.width, '260px');
  assert.equal(f.cards[1].style.left, '270px');
  f.resize(529, 700);
  f.flush();
  assert.deepEqual(f.cards.map(card => card.style.left), ['0px', '0px', '0px']);
  assert.deepEqual(f.cards.map(card => card.style.top), ['0px', '410px', '670px']);
  assert.equal(f.cards[0].style.width, '529px');
  f.resize(808, 1000);
  f.flush();
  assert.deepEqual(f.cards.map(card => card.style.left), ['0px', '274px', '548px']);
  assert.equal(f.cards[0].style.width, '260px');
});

test('hidden channel aliases are measured when revealed and preserve saved fields', () => {
  const f = setup([400, 250], 814, 1400, true);
  assert.equal(f.other.style.height, undefined);
  f.cards[0].fields.alias = 'unsaved-edit';
  f.revealOther();
  f.flush();
  assert.equal(f.other.style.height, '250px');
  assert.equal(f.otherCards[0].style.width, '400px');
  assert.equal(f.cards[0].fields.alias, 'unsaved-edit');
});

test('a single alias model keeps the existing half-wide/full-compact policy', () => {
  const f = setup([200]);
  assert.equal(f.cards[0].style.width, '400px');
  f.resize(700, 1000);
  f.flush();
  assert.equal(f.cards[0].style.width, '700px');
});
