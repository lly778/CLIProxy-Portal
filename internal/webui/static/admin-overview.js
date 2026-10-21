(function () {
  "use strict";
  var card = document.querySelector("[data-overview-resources]");
  if (!card) return;
  var busy = false;
  var note = card.querySelector("[data-overview-poll-status]");
  function refresh() {
    if (busy || document.visibilityState === "hidden") return;
    busy = true;
    fetch("/admin/system/resources/status", {headers: {Accept: "application/json"}, credentials: "same-origin", cache: "no-store"})
      .then(function (response) {
        if (!response.ok || !(response.headers.get("content-type") || "").includes("application/json")) throw new Error("monitor");
        return response.json();
      })
      .then(function (value) {
        card.querySelectorAll("[data-overview-resource]").forEach(function (el) {
          el.textContent = value[el.dataset.overviewResource] || "—";
          if (el.dataset.overviewResource === "State") el.hidden = value.State === "正常";
        });
        note.textContent = value.Fresh ? "" : "监测数据暂不可用";
        note.hidden = value.Fresh;
      })
      .catch(function () { note.textContent = "状态暂时无法刷新"; note.hidden = false; })
      .finally(function () { busy = false; });
  }
  window.setInterval(refresh, 5000);
  document.addEventListener("visibilitychange", function () { if (document.visibilityState !== "hidden") refresh(); });
  refresh();
}());
