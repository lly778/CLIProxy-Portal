(function () {
  "use strict";

  // Keep DOM/keyboard order intact; place each next card in the shorter column.
  var masonryLayouts = [];
  function initMasonry(list, cardSelector, options) {
    if (!list || !window.matchMedia || !window.requestAnimationFrame) return;
    var cards = Array.prototype.slice.call(list.querySelectorAll(cardSelector));
    var pendingLayout = false;
    function layoutCards() {
      pendingLayout = false;
      var width = list.getBoundingClientRect().width;
      if (!width || !cards.length) return;
      var config = options(width, cards.length);
      var columns = config.columns;
      var gap = config.gap;
      var cardWidth = (width - gap * (columns - 1)) / columns;
      var heights = Array(columns).fill(0);
      cards.forEach(function (card) { card.style.width = cardWidth + "px"; });
      list.classList.add("is-masonry");
      cards.forEach(function (card) {
        var column = 0;
        for (var index = 1; index < columns; index++) {
          if (heights[index] < heights[column]) column = index;
        }
        card.style.left = column * (cardWidth + gap) + "px";
        card.style.top = heights[column] + "px";
        heights[column] += card.getBoundingClientRect().height + gap;
      });
      list.style.height = Math.max(0, Math.max.apply(null, heights) - gap) + "px";
    }
    function scheduleLayout() {
      if (pendingLayout) return;
      pendingLayout = true;
      window.requestAnimationFrame(layoutCards);
    }
    if (window.PortalMasonrySort) {
      window.PortalMasonrySort.init({
        list: list, cards: cards,
        applyOrder: function (order) { cards = order.slice(); layoutCards(); }
      });
    }
    layoutCards();
    masonryLayouts.push(scheduleLayout);
    window.addEventListener("resize", scheduleLayout);
    if (list.addEventListener) list.addEventListener("click", scheduleLayout);
    if (window.ResizeObserver) {
      var observer = new window.ResizeObserver(scheduleLayout);
      observer.observe(list);
      cards.forEach(function (card) { observer.observe(card); });
    }
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(scheduleLayout);
  }

  var narrowCards = window.matchMedia && window.matchMedia("(max-width: 760px)");
  function scheduleMasonryLayouts() { masonryLayouts.forEach(function (layout) { layout(); }); }
  if (narrowCards && narrowCards.addEventListener) narrowCards.addEventListener("change", scheduleMasonryLayouts);
  initMasonry(document.querySelector("[data-global-oauth-presets] .oauth-preset-list"), ".oauth-preset-row", function () {
    return { columns: narrowCards.matches ? 1 : 2, gap: 10 };
  });
})();
