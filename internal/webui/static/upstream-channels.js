(function () {
  "use strict";

  // Keep DOM/keyboard order intact; place each next card in the shorter column.
  var masonryLayouts = [];
  var masonrySorters = [];
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
      masonrySorters.push(window.PortalMasonrySort.init({
        list: list, cards: cards,
        applyOrder: function (order) { cards = order.slice(); layoutCards(); }
      }));
    }
    layoutCards();
    masonryLayouts.push(scheduleLayout);
    window.addEventListener("resize", scheduleLayout);
    // Alias buttons modify their card later in this event's bubbling phase.
    if (list.addEventListener) list.addEventListener("click", scheduleLayout);
    if (window.ResizeObserver) {
      var observer = new window.ResizeObserver(scheduleLayout);
      observer.observe(list);
      cards.forEach(function (card) { observer.observe(card); });
    }
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(scheduleLayout);
  }

  var narrowCards = window.matchMedia && window.matchMedia("(max-width: 760px)");
  var compactCards = window.matchMedia && window.matchMedia("(max-width: 1080px)");
  function scheduleMasonryLayouts() { masonryLayouts.forEach(function (layout) { layout(); }); }
  if (narrowCards && narrowCards.addEventListener) narrowCards.addEventListener("change", scheduleMasonryLayouts);
  if (compactCards && compactCards.addEventListener) compactCards.addEventListener("change", scheduleMasonryLayouts);
  initMasonry(document.querySelector("[data-global-oauth-presets] .oauth-preset-list"), ".oauth-preset-row", function () {
    return { columns: narrowCards.matches ? 1 : 2, gap: 10 };
  });
  document.querySelectorAll(".oauth-alias-list").forEach(function (list) {
    initMasonry(list, ".oauth-alias-row", function (width, count) {
      var gap = narrowCards.matches ? 10 : 14;
      // Match the existing auto-fit/minimum-260px policy, including one-card expansion.
      var columns = compactCards.matches ? Math.min(count, Math.max(1, Math.floor((width + gap) / (260 + gap)))) : 2;
      return { columns: columns, gap: gap };
    });
  });

  var page = document.querySelector("[data-upstream-channel-page]");
  if (!page || !window.history) return;
  var form = page.querySelector(".upstream-channel-form");
  var select = form && form.querySelector("select[name='channel']");
  var panels = Array.prototype.slice.call(page.querySelectorAll("[data-upstream-channel-panel]"));
  if (!select || !panels.length) return;
  var shownURL = window.location.href;
  var button = form.querySelector("button[type='submit']");
  if (button) button.remove();

  // Keep every panel and its editable forms mounted. A switch is entirely local.
  function switchChannel(targetURL, fromHistory) {
    var target = new URL(targetURL, shownURL);
    if (target.origin !== window.location.origin || target.pathname !== "/admin/upstreams") return;
    var channel = target.searchParams.get("channel") || "codex";
    var next = panels.find(function (panel) { return panel.getAttribute("data-channel") === channel; });
    if (!next) {
      select.value = page.getAttribute("data-channel");
      if (fromHistory) window.history.replaceState(null, "", shownURL);
      return;
    }
    masonrySorters.forEach(function (sorter) { sorter.cancel(); });
    panels.forEach(function (panel) { panel.hidden = panel !== next; });
    scheduleMasonryLayouts();
    var changed = channel !== page.getAttribute("data-channel");
    page.setAttribute("data-channel", channel);
    // The hidden field controls only the return location, never preset scope.
    page.querySelectorAll("[data-global-oauth-presets] input[name='channel']").forEach(function (input) { input.value = channel; });
    select.value = channel;
    if (!fromHistory && changed) window.history.pushState(null, "", target.href);
    shownURL = fromHistory || changed ? target.href : shownURL;
    var status = page.querySelector("[data-channel-switch-status]");
    if (status && changed) status.textContent = "已切换至 " + select.options[select.selectedIndex].text;
  }

  function switchFromForm() {
    var target = new URL(form.action, shownURL);
    target.search = "";
    target.searchParams.set("channel", select.value);
    switchChannel(target.href, false);
  }

  select.addEventListener("change", switchFromForm);
  form.addEventListener("submit", function (event) {
    event.preventDefault();
    switchFromForm();
  });
  window.addEventListener("popstate", function () {
    switchChannel(window.location.href, true);
  });
})();
