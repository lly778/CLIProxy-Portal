(function () {
  "use strict";
  var panel = document.querySelector("[data-resource-panel]");
  if (!panel) return;
  var dialog = panel.querySelector("#resource-settings-dialog");
  var form = panel.querySelector("[data-resource-settings]");
  panel.querySelector("[data-resource-open]").addEventListener("click", function () { if (!dialog.open) dialog.showModal(); });
  panel.querySelector("[data-resource-close]").addEventListener("click", function () { dialog.close(); });
  dialog.addEventListener("close", function () { form.reset(); });
  form.addEventListener("submit", function (event) {
    if (form.dataset.submitting === "true") { event.preventDefault(); return; }
    form.dataset.submitting = "true";
    form.querySelector('[type="submit"]').disabled = true;
  });
  window.addEventListener("pageshow", function () {delete form.dataset.submitting;form.querySelector('[type="submit"]').disabled=false;});
  var busy = false;
  var note = panel.querySelector("[data-resource-poll-status]");
  function text(selector, value) { var el = panel.querySelector(selector); if (el) el.textContent = value || ""; }
  function refresh() {
    if (busy || document.visibilityState === "hidden") return;
    busy = true;
    fetch("/admin/system/resources/status", {headers: {Accept: "application/json"}, credentials: "same-origin", cache: "no-store"})
      .then(function (r) { if (!r.ok || !(r.headers.get("content-type") || "").includes("application/json")) throw new Error("monitor"); return r.json(); })
      .then(function (v) {
        panel.querySelectorAll("[data-resource-field]").forEach(function (el) { var value = v[el.dataset.resourceField] || "0"; if (el.textContent !== value) el.textContent = value; });
        var state = panel.querySelector("[data-resource-limit-alert]"); state.className = "alert " + v.StateClass + " resource-limit-alert"; state.hidden = v.State === "正常";
        text("[data-resource-warning]", v.Warning);
        var revision = panel.querySelector('[data-resource-settings] [name="revision"]');
        note.textContent = revision && Number(revision.value) !== v.Policy.revision ? " · 设置已更新，请重新载入后保存" : "";
      })
      .catch(function () { note.textContent = " · 状态暂时无法刷新"; })
      .finally(function () { busy = false; });
  }
  window.setInterval(refresh, 5000);
  document.addEventListener("visibilitychange", function () { if (document.visibilityState !== "hidden") refresh(); });
}());
