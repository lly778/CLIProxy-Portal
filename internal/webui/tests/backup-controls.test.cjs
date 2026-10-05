const assert = require('node:assert/strict');
const test = require('node:test');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const code = fs.readFileSync(path.join(__dirname, '../static/backup-controls.js'), 'utf8');

function setup(busy = false, fetchReply = { ok: true }) {
  const timers = [];
  const pageEvents = {};
  const requests = [];
  const downloads = [];
  let reloads = 0;
  const dialogs = ['backup-run-dialog', 'backup-restore-dialog'].map(id => {
    const events = {};
    const makeForm = (settings) => {
      const formEvents = settings ? {} : events;
      const submit = { disabled: false, textContent: '确认' };
      const password = {value:'test-admin-password', valid:true, reportValidity(){return this.valid;}};
      const fields = {'[name="admin_password"]':password, '[name="csrf_token"]':{value:'test-csrf'}};
      const status = {textContent:''};
      const download = settings ? {disabled:false,textContent:'下载备份恢复密钥',addEventListener(_,fn){this.click=fn;}} : null;
      return { dataset: {}, enctype: id.includes('restore') ? 'multipart/form-data' : '', resets: 0,submit,events:formEvents,password,status,download,
        reset() { this.resets++; password.value=''; },
        querySelector(s) { if(s==='[type="submit"]')return submit; if(s==='[data-backup-download]')return download; if(s==='[data-backup-key-status]')return status; return fields[s]; },
        addEventListener(name, fn) { formEvents[name] = fn; } };
    };
    const form = makeForm(false);
    const forms = id.includes('run') ? [form,makeForm(true)] : [form];
    const submit = form.submit;
    const closeButton = { addEventListener(name, fn) { events.closeClick = fn; } };
    const closeEvents = [];
    return { id, open: false, form, forms, submit, events,
      querySelector(selector) { return selector === 'form' ? form : closeButton; },
      querySelectorAll(selector) { return selector==='form' ? forms : [closeButton]; },
      addEventListener(name, fn) { if(name==='close')closeEvents.push(fn); else events[name] = fn; },
      showModal() { this.open = true; }, close() { this.open = false; closeEvents.forEach(fn=>fn()); } };
  });
  const buttons = dialogs.map(dialog => ({ disabled: false, getAttribute() { return dialog.id; }, addEventListener(_, fn) { this.click = fn; } }));
  const section = { dataset: { backupBusy: String(busy) },
    querySelectorAll(s) { return s === '[data-backup-open]' ? buttons : dialogs; },
    querySelector() { return dialogs.find(d => d.open); } };
  const document = { body:{appendChild(){}}, createElement(){return {click(){downloads.push(this.download);},remove(){}};}, getElementById(id) { return id === 'backup' ? section : dialogs.find(d => d.id === id); } };
  const window = { fetch: async (...args) => { requests.push(args); if (fetchReply instanceof Error) throw fetchReply; if(typeof fetchReply==='function')return fetchReply(); return fetchReply; }, addEventListener(name, fn) { (pageEvents[name] ||= []).push(fn); }, setTimeout(fn) { timers.push(fn); }, location: { reload() { reloads++; } } };
  vm.runInNewContext(code, { document, window,URLSearchParams,Blob,URL:{createObjectURL(){return 'blob:test-key';},revokeObjectURL(){}} });
  return { buttons, dialogs, timers, pageEvents, requests,downloads,get reloads() { return reloads; } };
}

test('buttons open native dialogs, closing clears credentials', () => {
  const page = setup();
  page.buttons[0].disabled = true;
  page.buttons[0].click();
  assert.equal(page.dialogs[0].open, false);
  page.buttons[0].disabled = false;
  page.buttons[0].click();
  assert.equal(page.dialogs[0].open, true);
  page.dialogs[0].events.closeClick();
  assert.equal(page.dialogs[0].open, false);
  assert.equal(page.dialogs[0].form.resets, 1);
  assert.equal(page.dialogs[0].forms[1].resets, 1);
});

test('key download sends only verification fields and keeps configuration unchanged', async () => {
  const page=setup(false,{ok:true,redirected:false,headers:{get(){return 'application/octet-stream';}},text:async()=> 'a'.repeat(64)+'\n'});
  page.buttons[0].click();
  const form=page.dialogs[0].forms[1];
  await form.download.click();
  assert.deepEqual(page.downloads,['portal-recovery.key']);
  assert.equal(page.requests[0][0],'/admin/system/backup/key');
  const request=page.requests[0][1];
  assert.equal(request.method,'POST');
  assert.equal(request.cache,'no-store');
  assert.equal(request.body.get('admin_password'),'test-admin-password');
  assert.equal(request.body.get('csrf_token'),'test-csrf');
  assert.equal([...request.body.keys()].length,2);
  assert.equal(form.password.value,'');
  assert.equal(form.download.disabled,false);
  assert.match(form.status.textContent,/已下载/);
  assert.equal(form.dataset.submitting,undefined);
  assert.equal(form.dataset.downloading,undefined);
});

test('invalid credentials or response do not download an HTML error as a key', async () => {
  const page=setup(false,{ok:false});
  page.buttons[0].click();
  const form=page.dialogs[0].forms[1];
  form.password.valid=false;
  await form.download.click();
  assert.equal(page.requests.length,0);
  form.password.valid=true;
  await form.download.click();
  assert.equal(page.downloads.length,0);
  assert.match(form.status.textContent,/失败/);
  assert.equal(form.download.disabled,false);
  const invalid=setup(false,{ok:true,headers:{get(){return 'application/octet-stream';}},text:async()=> 'plaintext error'});
  invalid.buttons[0].click();
  await invalid.dialogs[0].forms[1].download.click();
  assert.equal(invalid.downloads.length,0);
});

test('closing the dialog during key verification clears credentials and prevents late download', async () => {
  let resolve;
  const reply=new Promise(r=>{resolve=r;});
  const page=setup(false,()=>reply);
  page.buttons[0].click();
  const form=page.dialogs[0].forms[1];
  const pending=form.download.click();
  let prevented=false;
  form.events.submit({preventDefault(){prevented=true;}});
  assert.equal(prevented,true);
  page.dialogs[0].close();
  resolve({ok:true,headers:{get(){return 'application/octet-stream';}},text:async()=> 'a'.repeat(64)+'\n'});
  await pending;
  assert.equal(page.downloads.length,0);
  assert.equal(form.password.value,'');
  assert.equal(form.download.disabled,false);
});

test('submission prevents duplicates and browser-back re-enables the form', () => {
  const page = setup();
  const dialog = page.dialogs[1];
  let prevented = 0;
  const event = { preventDefault() { prevented++; } };
  dialog.events.submit(event);
  assert.equal(dialog.submit.disabled, true);
  assert.equal(dialog.submit.textContent, '正在上传…');
  dialog.events.submit(event);
  assert.equal(prevented, 1);
  page.pageEvents.pageshow.forEach(fn => fn());
  assert.equal(dialog.submit.disabled, false);
  assert.equal(dialog.form.dataset.submitting, undefined);
});

test('busy status refresh does not interrupt an open dialog', async () => {
  const page = setup(true);
  page.buttons[1].click();
  await page.timers.shift()();
  assert.equal(page.reloads, 0);
  page.dialogs[1].events.closeClick();
  await page.timers.shift()();
  assert.equal(page.reloads, 1);
  assert.equal(setup().timers.length, 0);
});

test('restore outage retries without navigating to an error page', async () => {
  for (const reply of [{ ok: false }, new Error('offline')]) {
    const page = setup(true, reply);
    await page.timers.shift()();
    assert.equal(page.reloads, 0);
    assert.equal(page.timers.length, 1);
  }
});
