(function () {
  "use strict";

  var presetDialog = document.querySelector("[data-preset-editor]");
  if (presetDialog && presetDialog.showModal) {
    var presetForm = presetDialog.querySelector("[data-preset-editor-form]");
    var presetChannels = presetDialog.querySelector("[data-preset-editor-channels]");
    var presetStatus = presetDialog.querySelector("[data-preset-editor-status]");
    var presetTabs = presetDialog.querySelector("[data-preset-editor-tabs]");
    var presetSearch = presetDialog.querySelector("[data-preset-editor-search]");
    var presetState = null;
    var presetLoading = null;
    var presetSaving = false;

    function presetElement(tag, className, text) {
      var el = document.createElement(tag);
      if (className) el.className = className;
      if (text !== undefined) el.textContent = text;
      return el;
    }
    function presetMessage(text, error) {
      presetStatus.textContent = text || "";
      presetStatus.classList.toggle("is-error", !!error);
      if (error) presetStatus.focus();
    }
    function presetBusy(busy) {
      presetSaving = busy;
      presetDialog.querySelectorAll("[data-preset-editor-close]").forEach(function (button) { button.disabled = busy; });
      presetForm.querySelectorAll("button[type=submit]").forEach(function (button) { button.disabled = busy || !presetState; });
      presetForm.inert = busy;
    }
    function presetModelCard(model, channel) {
      var card = presetElement("article", "preset-editor-model");
      var toggle = presetElement("label", "preset-editor-toggle");
      var enabled = presetElement("input");
      enabled.type = "checkbox";
      enabled.checked = model.enabled;
      enabled.disabled = !!model.wildcard_rule;
      enabled.setAttribute("aria-label", "启用 " + channel.label + " " + model.id);
      toggle.append(enabled, presetElement("span", "mono", model.id));
      var modelName = presetElement("div", "preset-editor-model-name");
      modelName.append(toggle);
      if (model.wildcard_rule) modelName.append(presetElement("p", "muted small", "受规则限制：" + model.wildcard_rule));
      card.append(modelName);
      var fields = presetElement("div", "preset-editor-fields");
      var aliasField = presetElement("div", "preset-editor-alias-field");
      aliasField.append(presetElement("span", "preset-editor-field-caption", "别名"));
      var aliasControls = presetElement("div", "preset-editor-alias-controls");
      var aliases = presetElement("div", "preset-editor-aliases");
      var add = presetElement("button", "preset-editor-icon preset-editor-add", "+");
      add.type = "button";
      add.setAttribute("aria-label", "添加别名");
      add.title = "添加别名";
      function aliasRow(value) {
        var row = presetElement("div", "preset-editor-alias");
        var input = presetElement("input");
        input.type = "text";
        input.maxLength = 128;
        input.placeholder = "输入别名";
        input.value = value || "";
        row.classList.toggle("is-empty", !input.value);
        input.addEventListener("input", function () { row.classList.toggle("is-empty", !input.value); });
        input.setAttribute("aria-label", channel.label + " " + model.id + " 别名");
        var remove = presetElement("button", "preset-editor-icon", "×");
        remove.type = "button";
        remove.title = "移除别名";
        remove.setAttribute("aria-label", "移除 " + model.id + " 的这个别名");
        remove.addEventListener("click", function () {
          if (aliases.children.length === 1) { input.value = ""; row.classList.add("is-empty"); }
          else row.remove();
          add.disabled = aliases.children.length >= 32;
        });
        row.append(input, remove);
        aliases.append(row);
      }
      (model.aliases.length ? model.aliases : [""]).forEach(aliasRow);
      add.addEventListener("click", function () {
        if (aliases.children.length >= 32) return;
        aliasRow("");
        aliases.lastElementChild.querySelector("input").focus();
        add.disabled = aliases.children.length >= 32;
      });
      aliasControls.append(aliases, add);
      aliasField.append(aliasControls);
      fields.append(aliasField);
      var originalLabel = presetElement("label", "preset-editor-original");
      var original = presetElement("input");
      original.type = "checkbox";
      original.setAttribute("aria-label", "保留原名");
      original.checked = model.keep_original;
      originalLabel.append(original, presetElement("span", "", "保留原名"));
      fields.append(originalLabel);
      var cap = null;
      var levels = model.reasoning_levels || [];
      if (levels.length || model.reasoning_cap) {
        var capLabel = presetElement("label", "preset-editor-cap");
        capLabel.append(presetElement("span", "preset-editor-field-caption", "思考上限"));
        cap = presetElement("select");
        cap.setAttribute("aria-label", channel.label + " " + model.id + " 思考上限");
        var none = presetElement("option", "", "不限制");
        none.value = "";
        cap.append(none);
        levels.forEach(function (level) {
          var option = presetElement("option", "", (model.reasoning_labels || {})[level] || level);
          option.value = level;
          cap.append(option);
        });
        if (model.reasoning_cap && levels.indexOf(model.reasoning_cap) < 0) {
          var old = presetElement("option", "", model.reasoning_cap + "（当前不支持）");
          old.value = model.reasoning_cap;
          cap.append(old);
        }
        cap.value = model.reasoning_cap || "";
        capLabel.append(cap);
        fields.append(capLabel);
      } else {
        fields.append(presetElement("span", "preset-editor-no-cap muted", "—"));
      }
      card.append(fields);
      function syncEnabled() {
        card.classList.toggle("is-disabled", !enabled.checked);
        fields.querySelectorAll("input, select, button").forEach(function (input) { input.disabled = !enabled.checked; });
        add.disabled = !enabled.checked || aliases.children.length >= 32;
      }
      enabled.addEventListener("change", syncEnabled);
      syncEnabled();
      card.presetModel = { model: model, enabled: enabled, aliases: aliases, original: original, cap: cap };
      return card;
    }
    function presetFilterModels() {
      var term = presetSearch.value.trim().toLowerCase();
      presetChannels.querySelectorAll(".preset-editor-model").forEach(function (card) { card.hidden = card.presetModel.model.id.toLowerCase().indexOf(term) < 0; });
      presetChannels.querySelectorAll(".preset-editor-channel").forEach(function (section) {
        var matches = Array.from(section.querySelectorAll(".preset-editor-model")).some(function (card) { return !card.hidden; });
        section.querySelector(".preset-editor-empty").hidden = matches;
      });
    }
    presetSearch.addEventListener("input", presetFilterModels);
    presetTabs.addEventListener("keydown", function (event) {
      var tabs = Array.from(presetTabs.querySelectorAll("button"));
      var index = tabs.indexOf(event.target);
      if (index < 0 || ["ArrowLeft", "ArrowRight", "Home", "End"].indexOf(event.key) < 0) return;
      event.preventDefault();
      var target = event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : (index + (event.key === "ArrowLeft" ? -1 : 1) + tabs.length) % tabs.length;
      tabs[target].click();
      tabs[target].focus();
    });
    function presetPayload() {
      return { version: 3, channels: Array.from(presetChannels.children).map(function (section) {
        var target = { channel: section.presetChannel.channel, models: [], aliases: [], reasoning_caps: Object.create(null) };
        section.querySelectorAll(".preset-editor-model").forEach(function (card) {
          var row = card.presetModel;
          target.models.push({ id: row.model.id, enabled: row.enabled.checked, wildcard_rule: row.model.wildcard_rule || "" });
          if (!row.enabled.checked) return;
          var names = Array.from(row.aliases.querySelectorAll("input")).map(function (input) { return input.value.trim(); }).filter(Boolean);
          if (!row.original.checked && !names.length) throw new Error(row.model.id + " 不保留原名时，请至少设置一个别名");
          names.forEach(function (name) { target.aliases.push({ name: row.model.id, alias: name, fork: row.original.checked }); });
          if (row.cap && row.cap.value) target.reasoning_caps[row.model.id.toLowerCase()] = row.cap.value;
        });
        return target;
      }) };
    }
    document.addEventListener("click", async function (event) {
      var button = event.target.closest("[data-preset-edit]");
      if (!button) return;
      presetDialog.querySelector("#preset-editor-title").textContent = button.hasAttribute("data-preset-create") ? "新增调用预设" : "编辑调用预设";
      if (presetLoading) presetLoading.abort();
      var controller = new AbortController();
      presetLoading = controller;
      presetState = null;
      presetChannels.replaceChildren();
      presetTabs.replaceChildren();
      presetForm.reset();
      presetBusy(false);
      presetMessage("正在读取预设…", false);
      if (!presetDialog.open) presetDialog.showModal();
      try {
        var url = button.getAttribute("data-preset-edit");
        var response = await fetch(url, { credentials: "same-origin", cache: "no-store", signal: controller.signal, headers: { "Accept": "application/json" } });
        var data = await response.json();
        if (!response.ok) throw new Error(data.error || "读取预设失败");
        if (presetLoading !== controller || !presetDialog.open) return;
        presetState = { url: url, revision: data.revision };
        presetForm.elements.name.value = data.name;
        if (data.creating) {
          var quickName = button.closest(".oauth-preset-tools").querySelector("input[name=name]");
          if (quickName) presetForm.elements.name.value = quickName.value;
        }
        data.channels.forEach(function (channel, index) {
          var section = presetElement("section", "preset-editor-channel");
          section.presetChannel = channel;
          section.id = "preset-editor-channel-" + index;
          section.hidden = index !== 0;
          section.setAttribute("role", "tabpanel");
          section.setAttribute("aria-label", channel.label);
          var tab = presetElement("button", "preset-editor-tab" + (index === 0 ? " is-active" : ""), channel.label);
          tab.type = "button";
          tab.setAttribute("role", "tab");
          tab.setAttribute("aria-selected", index === 0 ? "true" : "false");
          tab.setAttribute("aria-controls", section.id);
          tab.tabIndex = index === 0 ? 0 : -1;
          tab.append(presetElement("span", "preset-editor-tab-count", String(channel.models.length)));
          tab.addEventListener("click", function () {
            Array.from(presetChannels.children).forEach(function (panel) { panel.hidden = panel !== section; });
            presetTabs.querySelectorAll("button").forEach(function (button) { button.classList.toggle("is-active", button === tab); button.setAttribute("aria-selected", button === tab ? "true" : "false"); button.tabIndex = button === tab ? 0 : -1; });
            presetChannels.scrollTop = 0;
          });
          presetTabs.append(tab);
          var tableHead = presetElement("div", "preset-editor-table-head");
          ["模型", "别名", "保留原名", "思考上限"].forEach(function (text) { tableHead.append(presetElement("span", "", text)); });
          section.append(tableHead);
          var models = presetElement("div", "preset-editor-models");
          channel.models.forEach(function (model) { models.append(presetModelCard(model, channel)); });
          section.append(models);
          var empty = presetElement("p", "preset-editor-empty muted", "没有匹配的模型");
          empty.hidden = channel.models.length > 0;
          section.append(empty);
          presetChannels.append(section);
        });
        presetMessage(data.notice || "", false);
        presetBusy(false);
        presetForm.elements.name.focus();
      } catch (error) {
        if (error.name !== "AbortError" && presetLoading === controller) presetMessage(error.message || "读取预设失败，请刷新页面后重试", true);
      }
    });
    presetDialog.querySelectorAll("[data-preset-editor-close]").forEach(function (button) {
      button.addEventListener("click", function () { if (!presetSaving) presetDialog.close(); });
    });
    presetDialog.addEventListener("cancel", function (event) { if (presetSaving) event.preventDefault(); });
    presetDialog.addEventListener("close", function () { if (presetLoading) presetLoading.abort(); presetState = null; });
    presetForm.addEventListener("submit", async function (event) {
      event.preventDefault();
      if (!presetState || presetSaving) return;
      var body = new URLSearchParams(new FormData(presetForm));
      try { body.set("payload", JSON.stringify(presetPayload())); }
      catch (error) { presetMessage(error.message, true); return; }
      body.set("revision", presetState.revision);
      body.set("action", event.submitter && event.submitter.value === "apply" ? "apply" : "save");
      presetBusy(true);
      presetMessage(body.get("action") === "apply" ? "正在保存并应用…" : "正在保存…", false);
      try {
        var response = await fetch(presetState.url, { method: "POST", credentials: "same-origin", body: body, headers: { "Accept": "application/json" } });
        var result = await response.json();
        if (result.revision) presetState.revision = result.revision;
        if (result.edit_url) {
          presetState.url = result.edit_url;
          presetDialog.querySelector("#preset-editor-title").textContent = "编辑调用预设";
        }
        if (!response.ok) throw new Error(result.error || "保存预设失败");
        window.location.assign(result.redirect);
      } catch (error) {
        presetBusy(false);
        presetMessage(error.message || "保存预设失败，请稍后重试", true);
      }
    });
  }

  var mobileNav = document.querySelector("[data-mobile-nav]");
  if (mobileNav) {
    var mobileNavStorageKey = "cliproxy-mobile-nav-scroll-left";
    var mobileNavMedia = window.matchMedia("(max-width: 760px)");
    var mobileNavSaveFrame = 0;
    var navControls = document.querySelector("[data-nav-controls]");
    var navPrevious = document.querySelector("[data-nav-previous]");
    var navNext = document.querySelector("[data-nav-next]");

    function updateNavArrows() {
      if (!navControls || !navPrevious || !navNext) return;
      var overflow = mobileNavMedia.matches && mobileNav.scrollWidth > navControls.clientWidth + 1;
      navPrevious.hidden = navNext.hidden = !overflow;
      navPrevious.disabled = mobileNav.scrollLeft <= 1;
      navNext.disabled = mobileNav.scrollLeft >= mobileNav.scrollWidth - mobileNav.clientWidth - 1;
    }

    function scrollNav(direction) {
      var reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      mobileNav.scrollBy({ left: direction * Math.max(80, mobileNav.clientWidth * .75), behavior: reduced ? "auto" : "smooth" });
    }

    if (navPrevious) navPrevious.addEventListener("click", function () { scrollNav(-1); });
    if (navNext) navNext.addEventListener("click", function () { scrollNav(1); });
    if (navControls) navControls.addEventListener("wheel", function (event) {
      if (!mobileNavMedia.matches || event.ctrlKey) return;
      // Wheel input belongs to the horizontal navigation while hovered, even
      // at either boundary or when every tab fits. Do not scroll the page.
      event.preventDefault();
      if (mobileNav.scrollWidth <= mobileNav.clientWidth + 1) return;
      var delta = Math.abs(event.deltaX) > Math.abs(event.deltaY) ? event.deltaX : event.deltaY;
      if (event.deltaMode === 1) delta *= 16;
      else if (event.deltaMode === 2) delta *= mobileNav.clientWidth;
      var target = Math.max(0, Math.min(mobileNav.scrollWidth - mobileNav.clientWidth, mobileNav.scrollLeft + delta));
      if (Math.abs(target - mobileNav.scrollLeft) < 1) return;
      mobileNav.scrollLeft = target;
    }, { passive: false });

    function saveMobileNavScroll() {
      if (!mobileNavMedia.matches) return;
      try {
        window.sessionStorage.setItem(mobileNavStorageKey, String(mobileNav.scrollLeft));
      } catch (_) {
        // sessionStorage may be unavailable in privacy-restricted browsers.
      }
    }

    function scheduleMobileNavSave() {
      updateNavArrows();
      if (mobileNavSaveFrame) return;
      mobileNavSaveFrame = window.requestAnimationFrame(function () {
        mobileNavSaveFrame = 0;
        saveMobileNavScroll();
      });
    }

    function restoreMobileNavScroll() {
      updateNavArrows();
      if (!mobileNavMedia.matches) return;
      try {
        var saved = Number(window.sessionStorage.getItem(mobileNavStorageKey));
        if (Number.isFinite(saved) && saved >= 0) mobileNav.scrollLeft = saved;
      } catch (_) {
        // Keep the browser's default position when storage is unavailable.
      }
      updateNavArrows();
    }

    restoreMobileNavScroll();
    window.requestAnimationFrame(restoreMobileNavScroll);
    mobileNav.addEventListener("scroll", scheduleMobileNavSave, { passive: true });
    mobileNav.addEventListener("click", function (event) {
      if (event.target.closest("a.nav-item")) saveMobileNavScroll();
    });
    window.addEventListener("pagehide", saveMobileNavScroll);
    window.addEventListener("pageshow", restoreMobileNavScroll);
    window.addEventListener("resize", updateNavArrows);
    if (window.ResizeObserver) {
      var navObserver = new window.ResizeObserver(updateNavArrows);
      navObserver.observe(mobileNav);
    }
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

  document.querySelectorAll("[data-usage-trend], [data-health-trend]").forEach(function (chart, chartIndex) {
    var svg = chart.querySelector("svg");
    var tooltip = chart.querySelector("[data-trend-tooltip]");
    var cursor = chart.querySelector(".trend-cursor");
    var points = Array.prototype.slice.call(chart.querySelectorAll("[data-trend-point]"));
    var dots = [];
    points.forEach(function (point, index) {
      point.querySelectorAll(".trend-dot").forEach(function (dot) { dots.push({ dot: dot, index: index }); });
    });
    var bars = Array.prototype.slice.call(chart.querySelectorAll(".trend-bar[data-bar-x]"));
    if (!svg || !tooltip || !cursor || !points.length) return;
    var plot = chart.querySelector("[data-trend-plot]");
    var clip = chart.querySelector("[data-trend-clip]");
    var clipTarget = chart.querySelector("[data-trend-clip-target]");
    if (!plot || !clip || !clipTarget) return;
    var clipID = "usage-trend-clip-" + chartIndex;
    clip.id = clipID;
    clipTarget.setAttribute("clip-path", "url(#" + clipID + ")");
    // Clip each complete stack to a rounded outer outline. A very thin top
    // segment must not shrink the whole column's corner radius to near zero.
    var stackOutlines = [];
    chart.querySelectorAll("[data-trend-bar-stack]").forEach(function (stack, stackIndex) {
      var stackClip = stack.querySelector("[data-trend-bar-clip]");
      var outline = stack.querySelector("[data-trend-bar-outline]");
      var target = stack.querySelector("[data-trend-bar-stack-target]");
      if (!stackClip || !outline || !target) return;
      var stackID = "trend-bar-clip-" + chartIndex + "-" + stackIndex;
      stackClip.id = stackID;
      target.setAttribute("clip-path", "url(#" + stackID + ")");
      stack.querySelectorAll(".trend-bar[data-bar-x]").forEach(function (bar) { bar.dataset.barStackSegment = "true"; });
      stackOutlines.push(outline);
    });
    var leftBound = 100, rightBound = 900;
    var scale = 1, offset = 0, drag = null;
    var touchGesture = null;
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
      bars.concat(stackOutlines).forEach(function (bar) {
        var x = Number(bar.dataset.barX), y = Number(bar.dataset.barY);
        var width = Number(bar.dataset.barWidth), height = Number(bar.dataset.barHeight);
        if (width <= 0 || height <= 0) { bar.setAttribute("d", ""); return; }
        // Internal stack boundaries stay square and contiguous. The shared
        // complete-column clip supplies the outer rounding across segments.
        if (bar.dataset.barStackSegment === "true" || bar.dataset.barSquare === "true") {
          bar.setAttribute("d", "M" + x + "," + y + " h" + width + " v" + height + " h-" + width + " Z");
          return;
        }
        // Round only the two top corners, keeping both bottom corners square
        // on the baseline. Both charts share a flatter 4px corner. Limit the
        // radius in screen space before converting to SVG coordinates so thin
        // bars cannot acquire tall, stretched arcs during resize or zoom.
        var radius = Math.min(4, width * xScale / 4, height * yScale / 4);
        var rx = radius / xScale, ry = radius / yScale;
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
      dots.forEach(function (entry) {
        var dot = entry.dot;
        dot.toggleAttribute("hidden", !showSymbols || !visible[entry.index]);
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
      var requestsValue = tooltip.querySelector("[data-trend-requests]");
      var tokensValue = tooltip.querySelector("[data-trend-tokens]");
      if (requestsValue) requestsValue.textContent = nearest.dataset.requests;
      if (tokensValue) tokensValue.textContent = nearest.dataset.tokens;
      tooltip.querySelectorAll("[data-trend-value]").forEach(function (value) {
        value.textContent = nearest.dataset[value.dataset.trendValue] || "—";
      });
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

    // Use touch events for mobile gestures and leave pointer events for mouse
    // and pen. A second finger cancels native page pinch; single-finger vertical
    // moves still scroll the page through touch-action: pan-y.
    function chartTouches(event) {
      return Array.prototype.filter.call(event.touches, function (touch) { return chart.contains(touch.target); });
    }
    function inPlot(pointer) {
      return pointer && pointer.x >= leftBound && pointer.x <= rightBound && pointer.y >= 24 && pointer.y <= 222;
    }
    function beginTouchPan(touch, moved) {
      var pointer = pointerPosition(touch);
      if (!pointer) return;
      touchGesture = { kind: "pan", id: touch.identifier, x: pointer.x, clientX: touch.clientX, clientY: touch.clientY, offset: offset, moved: moved };
    }
    function beginTouchPinch(touches, rebasing) {
      var first = pointerPosition(touches[0]), second = pointerPosition(touches[1]);
      if (!first || !second || (!rebasing && (!inPlot(first) || !inPlot(second)))) return false;
      touchGesture = { kind: "pinch", x: (first.x + second.x) / 2, distance: Math.max(1, Math.hypot(touches[1].clientX - touches[0].clientX, touches[1].clientY - touches[0].clientY)) };
      chart.classList.add("trend-dragging");
      hideTrendTooltip();
      return true;
    }
    chart.addEventListener("touchstart", function (event) {
      var touches = chartTouches(event);
      if (!touches.length) return;
      if (!zoomable) { showTrendTooltip(touches[0]); return; }
      if (touches.length >= 2) {
        if (beginTouchPinch(touches) && event.cancelable) event.preventDefault();
      } else if (inPlot(pointerPosition(touches[0]))) {
        beginTouchPan(touches[0], false);
      }
    }, { passive: false });
    chart.addEventListener("touchmove", function (event) {
      if (!zoomable) { hideTrendTooltip(); return; }
      if (!touchGesture || touchGesture.kind === "scroll") return;
      var touches = chartTouches(event);
      if (touches.length >= 2) {
        if (touchGesture.kind !== "pinch" && !beginTouchPinch(touches)) return;
        var first = pointerPosition(touches[0]), second = pointerPosition(touches[1]);
        if (!first || !second) return;
        if (event.cancelable) event.preventDefault();
        var center = (first.x + second.x) / 2;
        var distance = Math.max(1, Math.hypot(touches[1].clientX - touches[0].clientX, touches[1].clientY - touches[0].clientY));
        var newScale = Math.max(1, Math.min(maxScale, scale * distance / touchGesture.distance));
        offset = center - (touchGesture.x - offset) * newScale / scale;
        scale = newScale;
        touchGesture.x = center;
        touchGesture.distance = distance;
        updateViewport();
      } else if (touches.length === 1 && touchGesture.kind === "pan" && touchGesture.id === touches[0].identifier) {
        var touch = touches[0];
        var dx = touch.clientX - touchGesture.clientX, dy = touch.clientY - touchGesture.clientY;
        if (!touchGesture.moved) {
          if (Math.max(Math.abs(dx), Math.abs(dy)) < 6) return;
          if (Math.abs(dy) >= Math.abs(dx)) {
            touchGesture.kind = "scroll";
            hideTrendTooltip();
            return;
          }
          touchGesture.moved = true;
          chart.classList.add("trend-dragging");
        }
        var pointer = pointerPosition(touch);
        if (!pointer) return;
        if (event.cancelable) event.preventDefault();
        offset = touchGesture.offset + pointer.x - touchGesture.x;
        updateViewport();
      }
    }, { passive: false });
    chart.addEventListener("touchend", function (event) {
      if (!touchGesture) return;
      var touches = chartTouches(event);
      if (touches.length >= 2) {
        if (touchGesture.kind === "pinch") beginTouchPinch(touches, true);
        return;
      }
      if (touches.length === 1) {
        if (touchGesture.kind === "pinch") beginTouchPan(touches[0], true);
        return;
      }
      var tap = touchGesture.kind === "pan" && !touchGesture.moved;
      touchGesture = null;
      chart.classList.remove("trend-dragging");
      if (tap && event.changedTouches.length) showTrendTooltip(event.changedTouches[0]);
      else hideTrendTooltip();
    });
    chart.addEventListener("touchcancel", function () {
      touchGesture = null;
      chart.classList.remove("trend-dragging");
      hideTrendTooltip();
    });

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
      if (event.pointerType === "touch") return;
      if (event.target && event.target.closest && event.target.closest(".trend-axis-label, .trend-x-label")) return;
      if (!zoomable || event.button !== 0) return;
      var pointer = pointerPosition(event);
      if (!pointer || pointer.x < leftBound || pointer.x > rightBound || pointer.y < 24 || pointer.y > 222) return;
      drag = { id: event.pointerId, x: pointer.x, offset: offset };
      event.preventDefault();
      chart.setPointerCapture(event.pointerId);
      chart.classList.add("trend-dragging");
    });
    chart.addEventListener("pointermove", function (event) {
      if (event.pointerType === "touch") return;
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
    chart.addEventListener("selectstart", function (event) { if (drag) event.preventDefault(); });
    chart.addEventListener("dblclick", function (event) {
      if (event.target && event.target.closest && event.target.closest(".trend-axis-label, .trend-x-label")) return;
      if (!zoomable) return;
      var pointer = pointerPosition(event);
      if (!pointer || pointer.x < leftBound || pointer.x > rightBound || pointer.y < 24 || pointer.y > 222) return;
      scale = 1; offset = 0; updateViewport();
    });
    chart.addEventListener("pointerleave", function (event) { if (event.pointerType !== "touch") hideTrendTooltip(); });
    chart.addEventListener("pointercancel", function (event) { if (event.pointerType !== "touch") hideTrendTooltip(); });
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

  document.querySelectorAll("[data-usage-range-filter]").forEach(function (form) {
    var select = form.querySelector("select[name='range']");
    if (!select) return;
    var dateFields = form.querySelectorAll("[data-usage-custom-date]");
    function syncCustomDates() {
      var custom = select.value === "custom";
      dateFields.forEach(function (field) { field.hidden = !custom; });
    }
    select.addEventListener("change", syncCustomDates);
    syncCustomDates();
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
      var quotaPage = form.closest("[data-upstream-channel-page]");
      var quotaChannelPanel = form.closest("[data-upstream-channel-panel]");
      var quotaChannel = quotaChannelPanel && quotaChannelPanel.getAttribute("data-channel");
      var quotaPageURL = window.location.href;
      if (quotaChannelPanel) {
        var quotaURL = new URL(quotaPageURL);
        quotaURL.searchParams.set("channel", quotaChannel);
        quotaPageURL = quotaURL.href;
      }
      function quotaPageIsCurrent() {
        return !quotaPage || quotaPage === document.querySelector("[data-upstream-channel-page]");
      }
      var previousResult = form.querySelector(".quota-refresh-result");
      if (previousResult) previousResult.remove();
      button.disabled = true;
      button.textContent = "正在刷新…";

      function delay(ms) {
        return new Promise(function (resolve) { window.setTimeout(resolve, ms); });
      }

      function updateQuotaPool() {
        if (!quotaPageIsCurrent()) return Promise.resolve(false);
        return fetch(quotaPageURL, {
          credentials: "same-origin",
          cache: "no-store",
          headers: quotaChannelPanel ? { "Accept": "text/html", "X-Upstream-Channel-Only": "true" } : { "Accept": "text/html" }
        }).then(function (response) {
          if (!response.ok) throw new Error("HTTP " + response.status);
          return response.text();
        }).then(function (html) {
          if (!quotaPageIsCurrent()) return false;
          var page = new DOMParser().parseFromString(html, "text/html");
          var current = (quotaChannelPanel || document).querySelector(quotaPanelSelector);
          var nextScope = page;
          if (quotaChannelPanel) {
            nextScope = Array.prototype.find.call(page.querySelectorAll("[data-upstream-channel-panel]"), function (panel) {
              return panel.getAttribute("data-channel") === quotaChannel;
            });
          }
          var next = nextScope && nextScope.querySelector(quotaPanelSelector);
          if (!current || !next) throw new Error("quota pool missing");
          var running = next.getAttribute("data-refresh-running") === "true";
          var replacement = document.importNode(next, true);
          var carousel = quotaPanelSelector === "[data-quota-pool]" && window.QuotaCarousel;
          var selectedProvider = carousel && carousel.selected(current);
          if (carousel) carousel.destroy(current);
          current.replaceWith(replacement);
          if (carousel) carousel.init(replacement, selectedProvider);
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
        headers: quotaChannelPanel ? {
          "Accept": "text/html", "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8", "X-Upstream-Channel-Only": "true"
        } : { "Accept": "text/html", "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8" }
      }).then(function (response) {
        if (!response.ok) throw new Error("HTTP " + response.status);
        return pollQuotaPool(0, 0);
      }).catch(function () {
        if (!quotaPageIsCurrent()) return;
        var currentButton = (quotaChannelPanel || document).querySelector(quotaPanelSelector + " [data-quota-refresh-form] button[type='submit']");
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
