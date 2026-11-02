(function () {
  "use strict";
  var dialog = document.querySelector("[data-preset-editor]");
  if (!dialog || !dialog.showModal) return;
  var form = dialog.querySelector("[data-preset-editor-form]");
  var picker = dialog.querySelector("[data-preset-picker]");
  var selected = dialog.querySelector("[data-preset-selected]");
  var search = dialog.querySelector("[data-preset-editor-search]");
  var status = dialog.querySelector("[data-preset-editor-status]");
  var catalogStatus = dialog.querySelector("[data-preset-catalog-status]");
  var state = null;
  var loading = null;
  var saving = false;
  var opener = null;

  function element(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }
  function message(text, error) {
    status.textContent = text || "";
    status.classList.toggle("is-error", !!error);
    if (error) status.focus();
  }
  function busy(value) {
    saving = value;
    form.inert = value;
    dialog.querySelectorAll("[data-preset-editor-close]").forEach(function (button) { button.disabled = value; });
    form.querySelectorAll("button[type=submit]").forEach(function (button) { button.disabled = value || !state; });
  }
  function models() {
    return state ? state.channels.flatMap(function (channel) { return channel.rows; }) : [];
  }
  function filter() {
    var term = search.value.trim().toLowerCase();
    var shown = 0;
    models().forEach(function (row) {
      row.label.hidden = (row.model.id + " " + row.channel.label).toLowerCase().indexOf(term) < 0;
      if (!row.label.hidden) shown++;
    });
    if (state) {
      state.channels.forEach(function (channel) { channel.group.hidden = channel.rows.every(function (row) { return row.label.hidden; }); });
      catalogStatus.textContent = "显示 " + shown + " / 共 " + models().length + " 个模型";
    }
  }
  function detail(row) {
    if (row.card) return row.card;
    var card = element("article", "client-model-draft-row upstream-routing-model");
    var heading = element("div", "client-model-draft-heading");
    var identity = element("div", "upstream-routing-identity");
    identity.append(element("strong", "mono", row.model.id), element("span", "badge neutral", row.channel.label));
    var remove = element("button", "client-model-remove", "×");
    remove.type = "button";
    remove.setAttribute("aria-label", "停用 " + row.channel.label + " " + row.model.id);
    remove.title = "从已选模型中移除";
    remove.addEventListener("click", function () { row.enabled.checked = false; renderSelected(); });
    heading.append(identity, remove);
    var aliasesField = element("div", "upstream-routing-alias-field");
    aliasesField.appendChild(element("span", "muted small", "用户侧别名"));
    row.aliases = element("div", "upstream-routing-alias-entries");
    var add = element("button", "upstream-routing-alias-add", "＋ 添加别名");
    add.type = "button";
    function alias(value) {
      var entry = element("div", "upstream-routing-alias-entry");
      var field = element("label", "upstream-routing-alias-input");
      var input = element("input");
      input.type = "text";
      input.value = value || "";
      input.maxLength = 128;
      input.placeholder = "用户侧模型名";
      input.autocomplete = "off";
      input.setAttribute("aria-label", row.channel.label + " " + row.model.id + " 的别名");
      field.appendChild(input);
      var removeAlias = element("button", "upstream-routing-alias-icon-button", "−");
      removeAlias.type = "button";
      removeAlias.setAttribute("aria-label", "移除 " + row.model.id + " 的这个别名");
      removeAlias.addEventListener("click", function () { entry.remove(); add.disabled = false; });
      entry.append(field, removeAlias);
      row.aliases.appendChild(entry);
      return input;
    }
    (row.model.aliases || []).forEach(alias);
    add.disabled = row.aliases.children.length >= 32;
    add.addEventListener("click", function () {
      if (row.aliases.children.length >= 32) return;
      alias("").focus();
      add.disabled = row.aliases.children.length >= 32;
    });
    aliasesField.append(row.aliases, add);
    var controls = element("div", "client-model-draft-controls");
    var original = element("label", "client-model-check");
    row.original = element("input");
    row.original.type = "checkbox";
    row.original.checked = row.model.keep_original;
    original.append(row.original, document.createTextNode("同时保留原名"));
    controls.appendChild(original);
    var levels = row.model.reasoning_levels || [];
    if (levels.length || row.model.reasoning_cap) {
      var field = element("label", "client-model-effort-field");
      field.appendChild(element("span", "", "思考强度上限"));
      row.cap = element("select");
      row.cap.setAttribute("aria-label", row.channel.label + " " + row.model.id + " 的思考强度上限");
      var none = element("option", "", "不限制");
      none.value = "";
      row.cap.appendChild(none);
      levels.forEach(function (level) {
        var option = element("option", "", (row.model.reasoning_labels || {})[level] || level);
        option.value = level;
        row.cap.appendChild(option);
      });
      if (row.model.reasoning_cap && levels.indexOf(row.model.reasoning_cap) < 0) {
        var old = element("option", "", row.model.reasoning_cap + "（当前不支持）");
        old.value = row.model.reasoning_cap;
        row.cap.appendChild(old);
      }
      row.cap.value = row.model.reasoning_cap || "";
      field.appendChild(row.cap);
      controls.appendChild(field);
    } else {
      controls.appendChild(element("span", "muted small", "该模型暂不支持设置思考上限"));
    }
    card.append(heading, aliasesField, controls);
    row.card = card;
    return card;
  }
  function renderSelected() {
    var fragment = document.createDocumentFragment();
    var count = 0;
    models().forEach(function (row) {
      if (row.enabled.checked) { fragment.appendChild(detail(row)); count++; }
    });
    if (!count) fragment.appendChild(element("p", "client-model-draft-empty muted", "从左侧勾选要启用的模型，再设置别名和思考上限。"));
    var scrollTop = selected.scrollTop;
    selected.replaceChildren(fragment);
    selected.scrollTop = scrollTop;
    dialog.querySelector("[data-preset-selected-count]").textContent = String(count);
  }
  function renderChannels(data) {
    if (!Array.isArray(data.channels)) throw new Error("模型目录格式无效");
    state.channels = data.channels.map(function (channel) {
      if (!Array.isArray(channel.models)) throw new Error("模型目录格式无效");
      var group = element("section", "upstream-routing-picker-group");
      group.appendChild(element("h4", "muted small", channel.label + " · " + channel.models.length));
      var rows = channel.models.map(function (model) {
        var label = element("label", "client-model-check");
        var enabled = element("input");
        enabled.type = "checkbox";
        enabled.checked = model.enabled;
        enabled.disabled = !!model.wildcard_rule;
        enabled.setAttribute("aria-label", "启用 " + channel.label + " " + model.id);
        label.append(enabled, element("span", "mono", model.id));
        if (model.wildcard_rule) label.title = "受通配规则控制：" + model.wildcard_rule;
        var row = { channel: channel, model: model, label: label, enabled: enabled, card: null, cap: null };
        enabled.addEventListener("change", function () { renderSelected(); message("保存或应用后生效，取消不会改变原配置。"); });
        group.appendChild(label);
        return row;
      });
      picker.appendChild(group);
      return { channel: channel.channel, rows: rows, group: group };
    });
    renderSelected();
    filter();
  }
  function payload() {
    return { version: 3, channels: state.channels.map(function (channel) {
      var target = { channel: channel.channel, models: [], aliases: [], reasoning_caps: Object.create(null) };
      channel.rows.forEach(function (row) {
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
    if (!button || saving) return;
    opener = button;
    if (loading) loading.abort();
    var controller = new AbortController();
    loading = controller;
    state = null;
    form.reset();
    picker.replaceChildren();
    selected.replaceChildren();
    dialog.querySelector("[data-preset-selected-count]").textContent = "0";
    busy(false);
    message("");
    catalogStatus.textContent = "正在获取模型目录…";
    var live = button.hasAttribute("data-live-config");
    dialog.querySelector("#preset-editor-title").textContent = live ? "上游模型配置与调用预设" : "编辑调用预设";
    dialog.querySelector("[data-preset-editor-description]").textContent = "选择启用的模型，在右侧设置别名和思考强度上限；取消不会改变原配置。";
    dialog.querySelector("[data-preset-name-label]").textContent = live ? "预设名称（选填）" : "预设名称";
    dialog.querySelector("[data-preset-name-help]").textContent = live ? "保存预设需要填写名称；直接应用无需填写。" : "保存预设只更新预设；保存并应用还会更新当前调用配置。";
    form.querySelector("button[value=apply]").textContent = live ? "应用配置" : "保存并应用";
    if (!dialog.open) dialog.showModal();
    search.focus();
    try {
      var url = button.getAttribute("data-preset-edit");
      var response = await fetch(url, { credentials: "same-origin", cache: "no-store", signal: controller.signal, headers: { Accept: "application/json" } });
      var data = await response.json();
      if (!response.ok) throw new Error(data.error || "读取配置失败");
      if (loading !== controller || !dialog.open) return;
      state = { url: url, revision: data.revision, live: live, channels: [] };
      form.elements.name.value = data.name || "";
      renderChannels(data);
      message(data.notice || "保存或应用后生效，取消不会改变原配置。");
      busy(false);
    } catch (error) {
      if (loading === controller && error.name !== "AbortError") {
        state = null;
        busy(false);
        catalogStatus.textContent = "模型目录获取失败";
        message(error.message || "读取配置失败，请刷新配置后重试", true);
      }
    } finally {
      if (loading === controller) loading = null;
    }
  });
  search.addEventListener("input", filter);
  dialog.querySelectorAll("[data-preset-editor-close]").forEach(function (button) {
    button.addEventListener("click", function () { if (!saving) dialog.close(); });
  });
  dialog.addEventListener("cancel", function (event) { if (saving) event.preventDefault(); });
  dialog.addEventListener("close", function () {
    if (loading) loading.abort();
    loading = null;
    state = null;
    picker.replaceChildren();
    selected.replaceChildren();
    if (opener && opener.isConnected) opener.focus();
  });
  form.addEventListener("submit", async function (event) {
    event.preventDefault();
    if (!state || saving) return;
    var action = event.submitter && event.submitter.value === "apply" ? "apply" : "save";
    var saveNew = state.live && action === "save";
    if ((action === "save" || !state.live) && !form.elements.name.value.trim()) {
      message("保存调用预设前请填写预设名称。", true);
      form.elements.name.focus();
      return;
    }
    var body = new URLSearchParams(new FormData(form));
    try { body.set("payload", JSON.stringify(payload())); }
    catch (error) { message(error.message, true); return; }
    body.set("revision", saveNew ? "0" : state.revision);
    body.set("action", action);
    busy(true);
    message(action === "apply" ? "正在应用配置…" : "正在保存预设…");
    try {
      var url = saveNew ? "/admin/upstreams/presets/new/edit" : state.url;
      var response = await fetch(url, { method: "POST", credentials: "same-origin", body: body, headers: { Accept: "application/json" } });
      var result = await response.json();
      if (result.revision) state.revision = result.revision;
      if (result.edit_url) state.url = result.edit_url;
      if (!response.ok) throw new Error(result.error || "保存配置失败");
      window.location.assign(result.redirect);
    } catch (error) {
      busy(false);
      message(error.message || "保存配置失败，请稍后重试", true);
    }
  });
})();
