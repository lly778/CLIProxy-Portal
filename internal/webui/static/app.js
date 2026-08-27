(function () {
  "use strict";

  var mobileNav = document.querySelector("[data-mobile-nav]");
  if (mobileNav) {
    var mobileNavStorageKey = "cliproxy-mobile-nav-scroll-left";
    var mobileNavMedia = window.matchMedia("(max-width: 760px)");
    var mobileNavSaveFrame = 0;

    function saveMobileNavScroll() {
      if (!mobileNavMedia.matches) return;
      try {
        window.sessionStorage.setItem(mobileNavStorageKey, String(mobileNav.scrollLeft));
      } catch (_) {
        // sessionStorage may be unavailable in privacy-restricted browsers.
      }
    }

    function scheduleMobileNavSave() {
      if (mobileNavSaveFrame) return;
      mobileNavSaveFrame = window.requestAnimationFrame(function () {
        mobileNavSaveFrame = 0;
        saveMobileNavScroll();
      });
    }

    function restoreMobileNavScroll() {
      if (!mobileNavMedia.matches) return;
      try {
        var saved = Number(window.sessionStorage.getItem(mobileNavStorageKey));
        if (Number.isFinite(saved) && saved >= 0) mobileNav.scrollLeft = saved;
      } catch (_) {
        // Keep the browser's default position when storage is unavailable.
      }
    }

    restoreMobileNavScroll();
    window.requestAnimationFrame(restoreMobileNavScroll);
    mobileNav.addEventListener("scroll", scheduleMobileNavSave, { passive: true });
    mobileNav.addEventListener("click", function (event) {
      if (event.target.closest("a.nav-item")) saveMobileNavScroll();
    });
    window.addEventListener("pagehide", saveMobileNavScroll);
    window.addEventListener("pageshow", restoreMobileNavScroll);
    if (mobileNavMedia.addEventListener) {
      mobileNavMedia.addEventListener("change", restoreMobileNavScroll);
    } else if (mobileNavMedia.addListener) {
      mobileNavMedia.addListener(restoreMobileNavScroll);
    }
  }

  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    var area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    try { document.execCommand("copy"); } finally { document.body.removeChild(area); }
    return Promise.resolve();
  }

  document.querySelectorAll("[data-usage-trend]").forEach(function (chart) {
    var svg = chart.querySelector("svg");
    var tooltip = chart.querySelector("[data-trend-tooltip]");
    var cursor = chart.querySelector(".trend-cursor");
    var points = Array.prototype.slice.call(chart.querySelectorAll("[data-trend-point]"));
    if (!svg || !tooltip || !cursor || !points.length) return;

    function hideTrendTooltip() {
      tooltip.hidden = true;
      cursor.hidden = true;
    }

    function showTrendTooltip(event) {
      var svgRect = svg.getBoundingClientRect();
      if (!svgRect.width) return;
      var viewX = (event.clientX - svgRect.left) * 1000 / svgRect.width;
      var nearest = points[0];
      points.forEach(function (point) {
        if (Math.abs(Number(point.dataset.x) - viewX) < Math.abs(Number(nearest.dataset.x) - viewX)) nearest = point;
      });
      var x = Number(nearest.dataset.x);
      var y = Math.min(Number(nearest.dataset.y), Number(nearest.dataset.tokenY));
      tooltip.querySelector("[data-trend-date]").textContent = nearest.dataset.date;
      tooltip.querySelector("[data-trend-requests]").textContent = nearest.dataset.requests;
      tooltip.querySelector("[data-trend-tokens]").textContent = nearest.dataset.tokens;
      cursor.setAttribute("x1", x);
      cursor.setAttribute("x2", x);
      cursor.hidden = false;
      tooltip.hidden = false;

      var chartRect = chart.getBoundingClientRect();
      var tooltipRect = tooltip.getBoundingClientRect();
      var pointX = svgRect.left - chartRect.left + x / 1000 * svgRect.width;
      var pointY = svgRect.top - chartRect.top + y / 260 * svgRect.height;
      var left = pointX + 14;
      if (left + tooltipRect.width > chart.clientWidth - 8) left = pointX - tooltipRect.width - 14;
      left = Math.max(8, Math.min(left, chart.clientWidth - tooltipRect.width - 8));
      var top = Math.max(8, Math.min(pointY - tooltipRect.height / 2, chart.clientHeight - tooltipRect.height - 8));
      tooltip.style.left = left + "px";
      tooltip.style.top = top + "px";
    }

    chart.addEventListener("pointermove", showTrendTooltip);
    chart.addEventListener("pointerleave", hideTrendTooltip);
    chart.addEventListener("pointercancel", hideTrendTooltip);
  });

  document.addEventListener("click", function (event) {
    var button = event.target.closest("[data-copy-target]");
    if (!button) return;
    var target = document.getElementById(button.getAttribute("data-copy-target"));
    if (!target) return;
    var original = button.textContent;
    copyText(target.textContent.trim()).then(function () {
      button.textContent = "已复制";
      button.classList.add("copied");
      window.setTimeout(function () { button.textContent = original; button.classList.remove("copied"); }, 1800);
    });
  });

  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (form.matches("[data-model-test-form]")) {
      event.preventDefault();
      var card = form.closest(".model-card");
      var result = card && card.querySelector("[data-model-test-result]");
      var button = form.querySelector("button[type='submit']");
      if (!result || !button || button.disabled) return;
      var original = button.textContent;
      button.disabled = true;
      button.textContent = "测试中…";

      function show(data) {
        result.hidden = false;
        result.classList.remove("success", "warning", "danger");
        result.classList.add(data.status || "danger");
        result.querySelector("[data-test-label]").textContent = data.statusLabel || "连接失败";
        result.querySelector("[data-test-latency]").textContent = data.latency || "";
        result.querySelector("[data-test-message]").textContent = data.message || "模型测试失败";
        result.querySelector("[data-test-time]").textContent = data.testedAt ? "测试于 " + data.testedAt : "";
      }

      fetch(form.action, {
        method: "POST",
        body: new URLSearchParams(new FormData(form)),
        credentials: "same-origin",
        headers: { "Accept": "application/json", "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8" }
      }).then(function (response) {
        return response.json().then(function (data) {
          if (!response.ok && !data.message) throw new Error("HTTP " + response.status);
          return data;
        });
      }).then(show).catch(function () {
        show({ status: "danger", statusLabel: "请求失败", message: "无法完成测试，请检查网络后重试。" });
      }).finally(function () {
        button.disabled = false;
        button.textContent = original;
      });
      return;
    }
    var message = form.getAttribute("data-confirm");
    if (message && !window.confirm(message)) event.preventDefault();
  });
})();
