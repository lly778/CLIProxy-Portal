(function () {
  "use strict";
  var section = document.querySelector("[data-client-model-settings]");
  if (!section) return;
  var dialog = section.querySelector("[data-client-model-dialog]");
  var rows = section.querySelector("[data-client-model-rows]");
  if (!dialog || !rows) return;
  var picker = dialog.querySelector("[data-client-model-picker-list]");
  var selectedList = dialog.querySelector("[data-client-model-draft-list]");
  var status = dialog.querySelector("[data-client-model-picker-status]");
  var catalogStatus = dialog.querySelector("[data-client-model-catalog-status]");
  var search = dialog.querySelector("[data-client-model-search]");
  var defaultModel = section.querySelector("[data-client-default-model]");
  var options = section.querySelector("[data-client-model-effort-options]");
  var opener = section.querySelector("[data-client-model-open]");
  var form = section.querySelector("#client-model-form");
  var saveButton = dialog.querySelector("[data-client-model-apply]");
  var draft = new Map();
  var remembered = new Map();
  var draftDefault = "";
  var controller = null;

  function readSaved() {
    draft = new Map(); remembered = new Map();
    rows.querySelectorAll("[data-client-model-row]").forEach(function (row) {
      var id = row.querySelector("[name='client_model_id']").value;
      var effort = row.querySelector("[data-client-model-effort]").value;
      draft.set(id, effort); remembered.set(id, effort);
    });
    draftDefault = defaultModel.value;
  }
  function checks() { return Array.from(picker.querySelectorAll("input[type='checkbox']")); }
  function updateDefaultMarkers() {
    selectedList.querySelectorAll("[data-draft-model]").forEach(function (row) {
      var selected = row.getAttribute("data-draft-model") === draftDefault;
      row.classList.toggle("is-default", selected);
      row.querySelector("input[type='radio']").checked = selected;
    });
  }
  function filter() {
    var query = search.value.trim().toLowerCase();
    var shown = 0;
    checks().forEach(function (input) {
      input.checked = draft.has(input.value);
      input.closest("label").hidden = input.value.toLowerCase().indexOf(query) < 0;
      if (!input.closest("label").hidden) shown++;
    });
    catalogStatus.textContent = "显示 " + shown + " / 共 " + checks().length + " 个模型";
  }
  function renderSelected() {
    var fragment = document.createDocumentFragment();
    draft.forEach(function (effort, id) {
      var row = document.createElement("article");
      row.className = "client-model-draft-row"; row.setAttribute("data-draft-model", id);
      var heading = document.createElement("div"); heading.className = "client-model-draft-heading";
      var name = document.createElement("strong"); name.className = "mono"; name.textContent = id;
      var remove = document.createElement("button"); remove.type = "button"; remove.className = "client-model-remove";
      remove.setAttribute("data-client-model-remove", ""); remove.setAttribute("aria-label", "移除模型 " + id); remove.title = "移除模型"; remove.textContent = "×";
      heading.append(name, remove);
      var controls = document.createElement("div"); controls.className = "client-model-draft-controls";
      var choice = document.createElement("label"); choice.className = "client-model-check";
      var radio = document.createElement("input"); radio.type = "radio"; radio.name = "client-model-default-choice"; radio.value = id;
      radio.setAttribute("aria-label", "设置 " + id + " 为默认模型");
      choice.append(radio, document.createTextNode("默认模型"));
      var field = document.createElement("label"); field.className = "client-model-effort-field";
      var caption = document.createElement("span"); caption.textContent = "默认思考强度";
      var select = document.createElement("select"); select.setAttribute("aria-label", id + " 的默认思考强度");
      select.appendChild(options.content.cloneNode(true)); select.value = effort;
      field.append(caption, select); controls.append(choice, field); row.append(heading, controls); fragment.appendChild(row);
    });
    if (!draft.size) {
      var empty = document.createElement("p"); empty.className = "client-model-draft-empty muted"; empty.textContent = "从全部模型中勾选要下发的模型。";
      fragment.appendChild(empty);
    }
    selectedList.replaceChildren(fragment);
    dialog.querySelector("[data-client-model-draft-count]").textContent = String(draft.size);
    updateDefaultMarkers();
    status.textContent = draft.size ? "保存后立即生效，取消不会改变原配置。" : "请选择要下发的模型。";
  }
  async function load() {
    if (controller) controller.abort();
    var request = new AbortController(); controller = request;
    catalogStatus.textContent = "正在获取完整模型目录…";
    try {
      var settings = { credentials: "same-origin", cache: "no-store", headers: { Accept: "application/json" }, signal: request.signal };
      var endpoint = "/admin/upstreams/client-models/catalog";
      var response = await fetch(endpoint, settings);
      if (!response.ok || !(response.headers.get("Content-Type") || "").includes("application/json")) throw new Error("目录获取失败");
      var data = await response.json();
      if (controller !== request || !dialog.open) return;
      if (!Array.isArray(data.models) || data.models.length > 10000) throw new Error("模型目录格式无效");
      var ids = []; var seen = new Set();
      data.models.concat(Array.from(draft.keys())).forEach(function (id) {
        if (typeof id !== "string" || !/^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$/.test(id) || seen.has(id.toLowerCase())) return;
        seen.add(id.toLowerCase()); ids.push(id);
      });
      var fragment = document.createDocumentFragment();
      ids.forEach(function (id) {
        var label = document.createElement("label"); label.className = "client-model-check";
        var input = document.createElement("input"); input.type = "checkbox"; input.value = id;
        var name = document.createElement("span"); name.className = "mono"; name.textContent = id;
        label.append(input, name); fragment.appendChild(label);
      });
      picker.replaceChildren(fragment); filter();
    } catch (error) {
      if (controller === request && error.name !== "AbortError") catalogStatus.textContent = "获取失败，请刷新配置后重新打开弹窗。已选模型仍可编辑。";
    } finally {
      if (controller === request) controller = null;
    }
  }
  opener.addEventListener("click", function () {
    readSaved(); search.value = ""; picker.replaceChildren(); renderSelected();
    dialog.showModal(); search.focus(); load();
  });
  search.addEventListener("input", function () { if (!controller) filter(); });
  picker.addEventListener("change", function (event) {
    var input = event.target;
    if (input.type !== "checkbox") return;
    if (input.checked) {
      if (draft.size >= 128) { input.checked = false; status.textContent = "最多选择 128 个模型。"; return; }
      draft.set(input.value, remembered.get(input.value) || "");
    } else {
      remembered.set(input.value, draft.get(input.value) || ""); draft.delete(input.value);
      if (draftDefault === input.value) draftDefault = "";
    }
    renderSelected();
  });
  selectedList.addEventListener("change", function (event) {
    var row = event.target.closest("[data-draft-model]"); if (!row) return;
    var id = row.getAttribute("data-draft-model");
    if (event.target.matches("select")) { draft.set(id, event.target.value); remembered.set(id, event.target.value); }
    else if (event.target.type === "radio") { draftDefault = id; updateDefaultMarkers(); }
    status.textContent = "保存后立即生效，取消不会改变原配置。";
  });
  selectedList.addEventListener("click", function (event) {
    var button = event.target.closest("[data-client-model-remove]"); if (!button) return;
    var id = button.closest("[data-draft-model]").getAttribute("data-draft-model");
    remembered.set(id, draft.get(id) || ""); draft.delete(id); if (draftDefault === id) draftDefault = "";
    renderSelected(); if (!controller) filter();
  });
  dialog.querySelectorAll("[data-client-model-close]").forEach(function (button) {
    button.addEventListener("click", function () { dialog.close(); });
  });
  dialog.addEventListener("close", function () { if (controller) controller.abort(); opener.focus(); });
  saveButton.addEventListener("click", function () {
    if (saveButton.disabled) return;
    if (!draft.size) { status.textContent = "请至少选择一个要下发的模型。"; return; }
    if (!draft.has(draftDefault)) { status.textContent = "请在已选模型中指定一个默认模型。"; return; }
    var fragment = document.createDocumentFragment(); var index = 0;
    draft.forEach(function (effort, id) {
      var row = document.createElement("div"); row.setAttribute("data-client-model-row", "");
      [["client_model_id", id], ["include_model", id], ["default_" + index, effort]].forEach(function (entry) {
        var input = document.createElement("input"); input.type = "hidden"; input.name = entry[0]; input.value = entry[1];
        if (entry[0].indexOf("default_") === 0) input.setAttribute("data-client-model-effort", ""); row.appendChild(input);
      });
      fragment.appendChild(row); index++;
    });
    rows.replaceChildren(fragment); defaultModel.value = draftDefault;
    saveButton.disabled = true;
    saveButton.textContent = "正在保存…";
    status.textContent = "正在保存客户端配置…";
    form.requestSubmit();
  });
})();
