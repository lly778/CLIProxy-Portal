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

  document.addEventListener("click", function (event) {
    var addButton = event.target.closest("[data-alias-add]");
    if (addButton) {
      var controls = addButton.closest(".oauth-alias-controls");
      var entries = controls && controls.querySelector("[data-alias-entries]");
      var template = controls && controls.querySelector("[data-alias-template]");
      if (!entries || !template || entries.children.length >= 32) return;
      var entry = template.content.firstElementChild.cloneNode(true);
      entries.appendChild(entry);
      entry.querySelector("input").focus();
      addButton.disabled = entries.children.length >= 32;
      return;
    }
    var removeButton = event.target.closest("[data-alias-remove]");
    if (!removeButton) return;
    var removedEntry = removeButton.closest(".oauth-alias-entry");
    var removedControls = removeButton.closest(".oauth-alias-controls");
    if (!removedEntry || !removedControls) return;
    removedEntry.remove();
    var nextAddButton = removedControls.querySelector("[data-alias-add]");
    if (nextAddButton) nextAddButton.disabled = false;
  });

  document.querySelectorAll(".oauth-alias-controls").forEach(function (controls) {
    var addButton = controls.querySelector("[data-alias-add]");
    var entries = controls.querySelector("[data-alias-entries]");
    if (addButton && entries) addButton.disabled = entries.children.length >= 32;
  });

  var requestModelTexts = Array.prototype.slice.call(document.querySelectorAll(".request-model-text"));
  var requestModelResizeFrame = 0;

  function fitRequestModelTitles() {
    requestModelTexts.forEach(function (model) {
      if (!model.dataset.fullText) {
        model.dataset.fullText = model.getAttribute("title") || model.textContent.trim();
      }
      model.removeAttribute("title");
      if (model.scrollHeight > model.clientHeight + 1) {
        model.setAttribute("title", model.dataset.fullText);
      }
    });
  }

  if (requestModelTexts.length) {
    fitRequestModelTitles();
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(fitRequestModelTitles);
    window.addEventListener("resize", function () {
      if (requestModelResizeFrame) window.cancelAnimationFrame(requestModelResizeFrame);
      requestModelResizeFrame = window.requestAnimationFrame(function () {
        requestModelResizeFrame = 0;
        fitRequestModelTitles();
      });
    });
  }

  var auditOverflowTexts = Array.prototype.slice.call(document.querySelectorAll(".audit-action-text, .audit-target-text, .audit-detail-text"));
  var auditOverflowResizeFrame = 0;

  function fitAuditOverflowTitles() {
    auditOverflowTexts.forEach(function (element) {
      element.removeAttribute("title");
      var visibleWidth = element.clientWidth;
      var visibleHeight = element.clientHeight;
      var previousLineClamp = element.style.webkitLineClamp;
      element.style.webkitLineClamp = "unset";
      var isOverflowing = element.scrollWidth > visibleWidth + 1 || element.scrollHeight > visibleHeight + 1;
      element.style.webkitLineClamp = previousLineClamp;
      if (isOverflowing) {
        element.setAttribute("title", element.textContent.trim());
      }
    });
  }

  if (auditOverflowTexts.length) {
    fitAuditOverflowTitles();
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(fitAuditOverflowTitles);
    window.addEventListener("resize", function () {
      if (auditOverflowResizeFrame) window.cancelAnimationFrame(auditOverflowResizeFrame);
      auditOverflowResizeFrame = window.requestAnimationFrame(function () {
        auditOverflowResizeFrame = 0;
        fitAuditOverflowTitles();
      });
    });
  }

  var requestStatusDetails = Array.prototype.slice.call(document.querySelectorAll(".request-status-detail"));
  var requestStatusResizeFrame = 0;

  function fitRequestStatusDetail(detail) {
    if (!detail.dataset.fullText) {
      detail.dataset.fullText = detail.getAttribute("title") || detail.textContent.trim();
    }
    var fullText = detail.dataset.fullText;
    var characters = Array.from(fullText);

    detail.removeAttribute("title");
    detail.textContent = fullText;
    detail.style.display = "block";
    detail.style.webkitLineClamp = "unset";

    if (detail.scrollHeight <= detail.clientHeight + 1) {
      detail.style.display = "";
      detail.style.webkitLineClamp = "";
      return;
    }

    var low = 0;
    var high = characters.length;
    while (low < high) {
      var middle = Math.ceil((low + high) / 2);
      detail.textContent = characters.slice(0, middle).join("").replace(/\s+$/, "") + "…";
      if (detail.scrollHeight <= detail.clientHeight + 1) {
        low = middle;
      } else {
        high = middle - 1;
      }
    }
    detail.textContent = characters.slice(0, low).join("").replace(/\s+$/, "") + "…";
    detail.setAttribute("title", fullText);
    detail.style.display = "";
    detail.style.webkitLineClamp = "";
  }

  function fitRequestStatusDetails() {
    requestStatusDetails.forEach(fitRequestStatusDetail);
  }

  if (requestStatusDetails.length) {
    fitRequestStatusDetails();
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(fitRequestStatusDetails);
    window.addEventListener("resize", function () {
      if (requestStatusResizeFrame) window.cancelAnimationFrame(requestStatusResizeFrame);
      requestStatusResizeFrame = window.requestAnimationFrame(function () {
        requestStatusResizeFrame = 0;
        fitRequestStatusDetails();
      });
    });
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

  document.querySelectorAll("[data-user-sort]").forEach(function (select) {
    select.addEventListener("change", function () {
      if (!select.form) return;
      if (select.form.requestSubmit) {
        select.form.requestSubmit();
      } else {
        select.form.submit();
      }
    });
  });

  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (form.matches("[data-quota-refresh-form]")) {
      event.preventDefault();
      var button = form.querySelector("button[type='submit']");
      if (!button || button.disabled) return;
      button.disabled = true;
      button.textContent = "正在刷新…";

      function delay(ms) {
        return new Promise(function (resolve) { window.setTimeout(resolve, ms); });
      }

      function updateQuotaPool() {
        return fetch(window.location.href, {
          credentials: "same-origin",
          cache: "no-store",
          headers: { "Accept": "text/html" }
        }).then(function (response) {
          if (!response.ok) throw new Error("HTTP " + response.status);
          return response.text();
        }).then(function (html) {
          var page = new DOMParser().parseFromString(html, "text/html");
          var current = document.querySelector("[data-quota-pool]");
          var next = page.querySelector("[data-quota-pool]");
          if (!current || !next) throw new Error("quota pool missing");
          var running = next.getAttribute("data-refresh-running") === "true";
          current.replaceWith(document.importNode(next, true));
          return running;
        });
      }

      function pollQuotaPool(attempt, failures) {
        return updateQuotaPool().then(function (running) {
          if (!running || attempt >= 180) return;
          return delay(1000).then(function () { return pollQuotaPool(attempt + 1, 0); });
        }).catch(function (error) {
          if (attempt >= 180 || failures >= 4) throw error;
          return delay(1500).then(function () { return pollQuotaPool(attempt + 1, failures + 1); });
        });
      }

      fetch(form.action, {
        method: "POST",
        body: new URLSearchParams(new FormData(form)),
        credentials: "same-origin",
        headers: { "Accept": "text/html", "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8" }
      }).then(function (response) {
        if (!response.ok) throw new Error("HTTP " + response.status);
        return pollQuotaPool(0, 0);
      }).catch(function () {
        var currentButton = document.querySelector("[data-quota-refresh-form] button[type='submit']");
        if (!currentButton) return;
        currentButton.disabled = false;
        currentButton.textContent = "刷新失败，请重试";
      });
      return;
    }
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
