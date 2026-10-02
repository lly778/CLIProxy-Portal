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

  function setLogTooltip(element, text) {
    if (text) {
      element.setAttribute("title", text);
      element.classList.add("log-tooltip");
      return;
    }
    element.removeAttribute("title");
    element.classList.remove("log-tooltip");
  }

  function fitRequestModelTitles() {
    requestModelTexts.forEach(function (model) {
      if (!model.dataset.fullText) {
        model.dataset.fullText = model.getAttribute("title") || model.textContent.trim();
      }
      setLogTooltip(model, model.scrollHeight > model.clientHeight + 1 ? model.dataset.fullText : "");
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
      setLogTooltip(element, "");
      var visibleWidth = element.clientWidth;
      var visibleHeight = element.clientHeight;
      var previousLineClamp = element.style.webkitLineClamp;
      element.style.webkitLineClamp = "unset";
      var isOverflowing = element.scrollWidth > visibleWidth + 1 || element.scrollHeight > visibleHeight + 1;
      element.style.webkitLineClamp = previousLineClamp;
      setLogTooltip(element, isOverflowing ? element.textContent.trim() : "");
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

    setLogTooltip(detail, "");
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
    setLogTooltip(detail, fullText);
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

  document.querySelectorAll("[data-usage-trend]").forEach(function (chart, chartIndex) {
    var svg = chart.querySelector("svg");
    var tooltip = chart.querySelector("[data-trend-tooltip]");
    var cursor = chart.querySelector(".trend-cursor");
    var points = Array.prototype.slice.call(chart.querySelectorAll("[data-trend-point]"));
    var dots = points.map(function (point) { return point.querySelector(".trend-dot.requests"); });
    var bars = Array.prototype.slice.call(chart.querySelectorAll(".trend-bar.tokens"));
    if (!svg || !tooltip || !cursor || !points.length) return;
    var plot = chart.querySelector("[data-trend-plot]");
    var clip = chart.querySelector("[data-trend-clip]");
    var clipTarget = chart.querySelector("[data-trend-clip-target]");
    if (!plot || !clip || !clipTarget) return;
    var clipID = "usage-trend-clip-" + chartIndex;
    clip.id = clipID;
    clipTarget.setAttribute("clip-path", "url(#" + clipID + ")");
    var leftBound = 100, rightBound = 900;
    var scale = 1, offset = 0, drag = null;
    var zoomable = points.length > 12;
    // Keep at least twelve actual points in every panned viewport. Use the
    // widest twelve intervals so rounded SVG coordinates cannot leave only 11.
    var minimumSpan = 0;
    for (var i = 12; i < points.length; i++) {
      minimumSpan = Math.max(minimumSpan, Number(points[i].dataset.x) - Number(points[i - 12].dataset.x));
    }
    var maxScale = zoomable ? (rightBound - leftBound) / minimumSpan : 1;
    if (zoomable) chart.classList.add("trend-interactive");
    function screenX(x) { return x * scale + offset; }
    function isVisibleX(x) {
      // SVG matrices round to float32; keep boundary-point decisions stable.
      return x >= leftBound - 0.001 && x <= rightBound + 0.001;
    }
    function updateTrendBars() {
      var matrix = svg.getScreenCTM();
      if (!matrix) return;
      var xScale = Math.hypot(matrix.a, matrix.b) * scale;
      var yScale = Math.hypot(matrix.c, matrix.d);
      if (!xScale || !yScale) return;
      bars.forEach(function (bar) {
        var x = Number(bar.dataset.barX), y = Number(bar.dataset.barY);
        var width = Number(bar.dataset.barWidth), height = Number(bar.dataset.barHeight);
        if (width <= 0 || height <= 0) { bar.setAttribute("d", ""); return; }
        // Round only the two top corners, keeping both bottom corners square
        // on the baseline. Top arcs remain screen-sized during zoom.
        var rx = Math.min(8 / xScale, width / 4), ry = Math.min(8 / yScale, height / 4);
        var right = x + width, bottom = y + height;
        bar.setAttribute("d", "M" + x + "," + bottom + " V" + (y + ry) + " A" + rx + "," + ry + " 0 0 1 " + (x + rx) + "," + y + " H" + (right - rx) + " A" + rx + "," + ry + " 0 0 1 " + right + "," + (y + ry) + " V" + bottom + " Z");
      });
    }
    function updateTrendSymbols() {
      var visible = points.map(function (point) {
        var x = screenX(Number(point.dataset.x));
        return isVisibleX(x);
      });
      var showSymbols = visible.filter(Boolean).length <= 36;
      dots.forEach(function (dot, index) {
        if (!dot) return;
        dot.toggleAttribute("hidden", !showSymbols || !visible[index]);
        // Cancel only horizontal stretching so marker size stays unchanged.
        dot.setAttribute("transform", "translate(" + dot.getAttribute("cx") + " 0) scale(" + (1 / scale) + " 1) translate(" + (-Number(dot.getAttribute("cx"))) + " 0)");
      });
    }
    function pointerPosition(event) {
      var matrix = svg.getScreenCTM();
      if (!matrix) return null;
      var pointer = svg.createSVGPoint();
      pointer.x = event.clientX;
      pointer.y = event.clientY;
      return pointer.matrixTransform(matrix.inverse());
    }

    // Text stays in HTML so mobile resizing does not shrink axis labels with
    // the SVG. Reduce only the displayed labels, never the underlying points.
    var labels = Array.prototype.slice.call(chart.querySelectorAll("[data-trend-label]"));
    function resizeTrendLabels() {
      var visible = labels.filter(function (label) {
        var x = screenX(Number(label.dataset.x));
        label.hidden = true;
        label.style.left = (x / 10) + "%";
        return isVisibleX(x);
      });
      if (!visible.length) return;
      var maxLabels = chart.clientWidth < 540 ? 3 : 6;
      var span = Number(visible[visible.length - 1].dataset.tick) - Number(visible[0].dataset.tick);
      var minimumStep = Math.max(1, Math.ceil(span / (maxLabels - 1)));
      var steps = [1, 2, 3, 4, 6, 8, 12, 24, 48, 72, 168];
      var step = steps.find(function (value) { return value >= minimumStep; }) || Math.ceil(minimumStep / 24) * 24;
      // Keep one anchored interval after zoom/pan. On text collisions, retry
      // the whole set instead of dropping middle labels and creating uneven gaps.
      for (var attempt = 0; attempt < 16; attempt++) {
        var previousRight = -Infinity, overlaps = false;
        visible.forEach(function (label) {
          label.hidden = Number(label.dataset.tick) % step !== 0;
          if (label.hidden) return;
          var width = label.getBoundingClientRect().width;
          var x = screenX(Number(label.dataset.x)) / 1000 * chart.clientWidth;
          // Omit clipped edges rather than moving labels off their data point.
          if (x - width / 2 < 4 || x + width / 2 > chart.clientWidth - 4) {
            label.hidden = true;
            return;
          }
          if (x - width / 2 < previousRight + 8) overlaps = true;
          label.style.left = x + "px";
          previousRight = x + width / 2;
        });
        if (!overlaps) break;
        step = steps.find(function (value) { return value > step; }) || step * 2;
      }
    }
    function resizeTrend() {
      resizeTrendLabels();
      updateTrendBars();
    }
    resizeTrend();
    updateTrendSymbols();
    if (window.ResizeObserver) {
      new ResizeObserver(resizeTrend).observe(chart);
    } else {
      window.addEventListener("resize", resizeTrend);
    }

    function hideTrendTooltip() {
      tooltip.hidden = true;
      cursor.setAttribute("hidden", "");
    }
    function updateViewport() {
      scale = Math.max(1, Math.min(maxScale, scale));
      offset = Math.max(rightBound - rightBound * scale, Math.min(leftBound - leftBound * scale, offset));
      plot.setAttribute("transform", "matrix(" + scale + " 0 0 1 " + offset + " 0)");
      updateTrendBars();
      updateTrendSymbols();
      resizeTrendLabels();
      hideTrendTooltip();
    }

    function showTrendTooltip(event) {
      var svgRect = svg.getBoundingClientRect();
      var matrix = svg.getScreenCTM();
      if (!svgRect.width || !matrix) return;
      var pointer = pointerPosition(event);
      if (!pointer || pointer.x < leftBound || pointer.x > rightBound || pointer.y < 24 || pointer.y > 222) {
        hideTrendTooltip();
        return;
      }
      var viewX = (pointer.x - offset) / scale;
      var nearest = null;
      points.forEach(function (point) {
        var x = Number(point.dataset.x);
        if (isVisibleX(screenX(x)) && (!nearest || Math.abs(x - viewX) < Math.abs(Number(nearest.dataset.x) - viewX))) nearest = point;
      });
      if (!nearest) { hideTrendTooltip(); return; }
      var x = screenX(Number(nearest.dataset.x));
      var y = Math.min(Number(nearest.dataset.y), Number(nearest.dataset.tokenY));
      tooltip.querySelector("[data-trend-date]").textContent = nearest.dataset.date;
      tooltip.querySelector("[data-trend-requests]").textContent = nearest.dataset.requests;
      tooltip.querySelector("[data-trend-tokens]").textContent = nearest.dataset.tokens;
      cursor.setAttribute("x1", x);
      cursor.setAttribute("x2", x);
      cursor.removeAttribute("hidden");
      tooltip.hidden = false;

      var chartRect = chart.getBoundingClientRect();
      var tooltipRect = tooltip.getBoundingClientRect();
      var position = svg.createSVGPoint();
      position.x = x;
      position.y = y;
      position = position.matrixTransform(matrix);
      var pointX = position.x - chartRect.left;
      var pointY = position.y - chartRect.top;
      var left = pointX + 14;
      if (left + tooltipRect.width > chart.clientWidth - 8) left = pointX - tooltipRect.width - 14;
      left = Math.max(8, Math.min(left, chart.clientWidth - tooltipRect.width - 8));
      var top = Math.max(8, Math.min(pointY - tooltipRect.height / 2, chart.clientHeight - tooltipRect.height - 8));
      tooltip.style.left = left + "px";
      tooltip.style.top = top + "px";
    }

    chart.addEventListener("wheel", function (event) {
      if (!zoomable) return;
      var pointer = pointerPosition(event);
      if (!pointer || pointer.x < leftBound || pointer.x > rightBound || pointer.y < 24 || pointer.y > 222) return;
      event.preventDefault();
      var delta = event.deltaY * (event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? chart.clientHeight : 1);
      var newScale = Math.max(1, Math.min(maxScale, scale * Math.exp(-delta * 0.002)));
      offset = pointer.x - (pointer.x - offset) * newScale / scale;
      scale = newScale;
      updateViewport();
    }, { passive: false });
    chart.addEventListener("pointerdown", function (event) {
      if (!zoomable || event.button !== 0) return;
      var pointer = pointerPosition(event);
      if (!pointer || pointer.x < leftBound || pointer.x > rightBound || pointer.y < 24 || pointer.y > 222) return;
      drag = { id: event.pointerId, x: pointer.x, offset: offset };
      chart.setPointerCapture(event.pointerId);
      chart.classList.add("trend-dragging");
    });
    chart.addEventListener("pointermove", function (event) {
      if (drag && drag.id === event.pointerId) {
        var pointer = pointerPosition(event);
        if (pointer) { offset = drag.offset + pointer.x - drag.x; updateViewport(); }
      } else {
        showTrendTooltip(event);
      }
    });
    function finishDrag(event) {
      if (!drag || drag.id !== event.pointerId) return;
      drag = null;
      chart.classList.remove("trend-dragging");
      if (chart.hasPointerCapture(event.pointerId)) chart.releasePointerCapture(event.pointerId);
      hideTrendTooltip();
    }
    chart.addEventListener("pointerup", finishDrag);
    chart.addEventListener("pointercancel", finishDrag);
    chart.addEventListener("lostpointercapture", finishDrag);
    chart.addEventListener("dblclick", function (event) {
      if (!zoomable) return;
      var pointer = pointerPosition(event);
      if (!pointer || pointer.x < leftBound || pointer.x > rightBound || pointer.y < 24 || pointer.y > 222) return;
      scale = 1; offset = 0; updateViewport();
    });
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
      var quotaPanelSelector = form.closest("[data-upstream-quotas]") ? "[data-upstream-quotas]" : "[data-quota-pool]";
      var previousResult = form.querySelector(".quota-refresh-result");
      if (previousResult) previousResult.remove();
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
          var current = document.querySelector(quotaPanelSelector);
          var next = page.querySelector(quotaPanelSelector);
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
        var currentButton = document.querySelector(quotaPanelSelector + " [data-quota-refresh-form] button[type='submit']");
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
