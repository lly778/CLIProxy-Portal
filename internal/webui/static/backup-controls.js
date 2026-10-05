(function () {
  "use strict";
  var section = document.getElementById("backup");
  if (!section) return;
  section.querySelectorAll("[data-backup-open]").forEach(function (button) {
    button.addEventListener("click", function () {
      var dialog = document.getElementById(button.getAttribute("data-backup-open"));
      if (!button.disabled && dialog && !dialog.open) dialog.showModal();
    });
  });
  section.querySelectorAll(".backup-dialog").forEach(function (dialog) {
    dialog.querySelectorAll("[data-backup-close]").forEach(function (button) {
      button.addEventListener("click", function () { dialog.close(); });
    });
    dialog.querySelectorAll("form").forEach(function (form) {
    var submit = form.querySelector('[type="submit"]');
    var label = submit.textContent;
    var initiallyDisabled = submit.disabled;
    window.addEventListener("pageshow", function () { delete form.dataset.submitting; submit.disabled = initiallyDisabled; submit.textContent = label; form.reset(); });
    dialog.addEventListener("close", function () { form.reset(); });
    form.addEventListener("submit", function (event) {
      if (form.dataset.submitting === "true" || form.dataset.downloading === "true") { event.preventDefault(); return; }
      form.dataset.submitting = "true";
      submit.disabled = true;
      submit.textContent = form.enctype === "multipart/form-data" ? "正在上传…" : "正在提交…";
    });
    var download = form.querySelector("[data-backup-download]");
    if (download) download.addEventListener("click", async function () {
      var password = form.querySelector('[name="admin_password"]');
      if (download.disabled || form.dataset.submitting === "true" || !password.reportValidity()) return;
      var status = form.querySelector("[data-backup-key-status]");
      var original = download.textContent;
      download.disabled = true;
      form.dataset.downloading = "true";
      download.textContent = "正在下载…";
      status.textContent = "正在验证管理员身份…";
      try {
        var fields = new URLSearchParams();
        fields.set("csrf_token", form.querySelector('[name="csrf_token"]').value);
        fields.set("admin_password", password.value);
        var response = await window.fetch("/admin/system/backup/key", {method:"POST", body:fields, credentials:"same-origin", cache:"no-store"});
        if (!response.ok || response.redirected || response.headers.get("Content-Type") !== "application/octet-stream") throw new Error("download failed");
        var key = await response.text();
        if (!/^[0-9a-f]{64}\n$/.test(key)) throw new Error("invalid key");
        if (!dialog.open) return;
        var objectURL = URL.createObjectURL(new Blob([key], {type:"application/octet-stream"}));
        try {
          var link = document.createElement("a");
          link.href = objectURL;
          link.download = "portal-recovery.key";
          document.body.appendChild(link);
          link.click();
          link.remove();
        } finally { window.setTimeout(function () { URL.revokeObjectURL(objectURL); }, 1000); }
        password.value = "";
        status.textContent = "恢复密钥已下载，请另行保管；保存设置时需再次验证管理员密码。";
      } catch (_) {
        if (dialog.open) status.textContent = "下载失败，请检查管理员密码或当前任务状态后重试。";
      } finally {
        delete form.dataset.downloading;
        download.disabled = false;
        download.textContent = original;
      }
    });
    });
  });
  async function refreshStatus() {
    if (section.dataset.backupBusy !== "true") return;
    if (section.querySelector("dialog[open]")) { window.setTimeout(refreshStatus, 5000); return; }
    // Database replacement briefly takes the service offline. Stay on the
    // current page and retry instead of navigating to an error page.
    try {
      var response = await window.fetch("/admin/system", { cache: "no-store", credentials: "same-origin" });
      if (response.ok && !section.querySelector("dialog[open]")) { window.location.reload(); return; }
    } catch (_) { /* Retry until the service returns. */ }
    window.setTimeout(refreshStatus, 5000);
  }
  if (section.dataset.backupBusy === "true") window.setTimeout(refreshStatus, 5000);
})();
