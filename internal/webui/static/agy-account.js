(() => {
  'use strict';
  const card = document.querySelector('[data-agy-account]');
  const dialog = card?.querySelector('[data-agy-account-dialog]');
  if (!dialog) return;
  const status = dialog.querySelector('[data-agy-account-status]');
  const form = dialog.querySelector('[data-agy-login-form]');
  let loginID = '';
  let busy = false;
  const cancelLogin = id => fetch('/admin/upstreams/agy/account/cancel', {
    method: 'POST', credentials: 'same-origin', keepalive: true,
    body: new URLSearchParams({ csrf_token: card.dataset.csrf, id }),
  }).catch(() => {});
  card.querySelector('[data-agy-account-open]')?.addEventListener('click', () => dialog.showModal());
  dialog.querySelector('[data-agy-account-close]').addEventListener('click', () => dialog.close());
  dialog.addEventListener('close', () => {
    if (loginID) cancelLogin(loginID);
    loginID = '';
    form.reset();
    form.hidden = true;
  });
  const request = async (action, fields = {}) => {
    if (busy) return null;
    busy = true;
    dialog.querySelectorAll('button').forEach(button => { button.disabled = true; });
    status.textContent = '正在处理，请稍候…';
    try {
      const response = await fetch(`/admin/upstreams/agy/account/${action}`, {
        method: 'POST', credentials: 'same-origin', headers: { Accept: 'application/json' },
        body: new URLSearchParams({ csrf_token: card.dataset.csrf, ...fields }),
      });
      const result = await response.json();
      if (!response.ok) throw new Error(result.error || '账号操作失败，请重试');
      status.textContent = result.message || '';
      return result;
    } catch (error) {
      status.textContent = error.message || '账号操作未完成，请重试';
      return null;
    } finally {
      busy = false;
      dialog.querySelectorAll('button').forEach(button => { button.disabled = false; });
    }
  };
  dialog.querySelector('[data-agy-login-start]').addEventListener('click', async () => {
    const result = await request('login');
    if (!result) return;
    const url = new URL(result.url);
    if (url.protocol !== 'https:' || url.hostname !== 'accounts.google.com') {
      status.textContent = '授权地址无效，请重新登录';
      return;
    }
    loginID = result.id;
    if (!dialog.open) { cancelLogin(loginID); loginID = ''; return; }
    form.hidden = false;
    form.querySelector('[data-agy-auth-link]').href = url.href;
    status.textContent = '打开授权页面，完成后将授权码或回调地址粘贴到这里。';
    form.elements.code.focus();
  });
  form.addEventListener('submit', async event => {
    event.preventDefault();
    const result = await request('finish', { id: loginID, code: form.elements.code.value.trim() });
    if (result) { form.reset(); window.location.reload(); }
  });
  dialog.querySelector('[data-agy-logout]')?.addEventListener('click', async () => {
    if (!window.confirm('退出当前 Google 账号并结束 AGY 运行会话？')) return;
    const result = await request('logout');
    if (result) window.location.reload();
  });
})();
