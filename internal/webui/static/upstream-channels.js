(function () {
  "use strict";

  // Keep DOM/keyboard order intact; place each next card in the shorter column.
  var presets = document.querySelector("[data-global-oauth-presets] .oauth-preset-list");
  if (presets && window.matchMedia && window.requestAnimationFrame) {
    var cards = Array.prototype.slice.call(presets.querySelectorAll(".oauth-preset-row"));
    var narrowPresets = window.matchMedia("(max-width: 760px)");
    var pendingLayout = false;
    function layoutPresets() {
      pendingLayout = false;
      var width = presets.getBoundingClientRect().width;
      if (!width || !cards.length) return;
      var columns = narrowPresets.matches ? 1 : 2;
      var gap = 10;
      var cardWidth = (width - gap * (columns - 1)) / columns;
      var heights = [0, 0];
      cards.forEach(function (card) { card.style.width = cardWidth + "px"; });
      presets.classList.add("is-masonry");
      cards.forEach(function (card) {
        var column = columns === 2 && heights[1] < heights[0] ? 1 : 0;
        card.style.left = column * (cardWidth + gap) + "px";
        card.style.top = heights[column] + "px";
        heights[column] += card.getBoundingClientRect().height + gap;
      });
      presets.style.height = Math.max(0, Math.max.apply(null, heights) - gap) + "px";
    }
    function schedulePresetLayout() {
      if (pendingLayout) return;
      pendingLayout = true;
      window.requestAnimationFrame(layoutPresets);
    }
    layoutPresets();
    window.addEventListener("resize", schedulePresetLayout);
    if (narrowPresets.addEventListener) narrowPresets.addEventListener("change", schedulePresetLayout);
    if (window.ResizeObserver) {
      var presetObserver = new window.ResizeObserver(schedulePresetLayout);
      presetObserver.observe(presets);
      cards.forEach(function (card) { presetObserver.observe(card); });
    }
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(schedulePresetLayout);
  }

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
    panels.forEach(function (panel) { panel.hidden = panel !== next; });
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
