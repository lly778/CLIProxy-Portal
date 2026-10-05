(function () {
  "use strict";

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
