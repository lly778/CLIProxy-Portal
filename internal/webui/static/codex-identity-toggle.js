(function () {
  "use strict";
  document.querySelectorAll("[data-codex-identity-toggle]").forEach(function (form) {
    var control = form.querySelector("input[name='enabled']");
    var status = form.querySelector("[data-identity-toggle-status]");
    if (!control || control.disabled || !status) return;
    var saved = control.checked;
    control.addEventListener("change", async function () {
      if (control.disabled) return;
      var body = new URLSearchParams(new FormData(form));
      control.disabled = true;
      status.textContent = "保存中";
      status.dataset.error = "false";
      try {
        var response = await fetch(form.action, {
          method: "POST", body: body, credentials: "same-origin",
          headers: { "Accept": "application/json" }
        });
        if (!response.ok) throw new Error(response.status === 403 ? "请求已失效，请刷新" : "保存失败，请重试");
        var result = await response.json();
        if (typeof result.enabled !== "boolean") throw new Error("保存失败，请刷新");
        saved = result.enabled;
        control.checked = saved;
        status.textContent = "已保存";
      } catch (error) {
        control.checked = saved;
        status.textContent = error.message === "请求已失效，请刷新" ? error.message : "保存失败，请重试";
        status.dataset.error = "true";
      } finally {
        control.disabled = false;
      }
    });
  });
})();
