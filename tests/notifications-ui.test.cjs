const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '../app/static/admin.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
class Element {
  constructor(id = '') {
    this.id = id; this.value = ''; this.textContent = ''; this.innerHTML = '';
    this.disabled = false; this.hidden = false; this.children = []; this.dataset = {};
    this.classList = {add() {}, remove() {}};
  }
  setAttribute() {}
  removeAttribute() {}
  replaceChildren(...children) { this.children = children; }
  closest() { return null; }
  querySelectorAll() { return []; }
  reset() {}
  focus() {}
}
const elements = new Map([...html.matchAll(/\bid="([^\"]+)"/g)].map(m => [m[1], new Element(m[1])]));
const calls = [];
let failMetadata = false;
let metadata = {enabled: true, receivers: [
  {id: 'dingtalk', name: '钉钉', configured: true},
  {id: 'wecom', name: '企业微信', configured: true},
  {id: 'bark', name: 'Bark', configured: false},
]};
let testResult = {ok: true, results: [{receiver: 'dingtalk', name: '钉钉', ok: true}]};
let testStatus = 200;
const ctx = vm.createContext({
  console, AbortController, setTimeout, clearTimeout,
  sessionStorage: {getItem() { return null; }, setItem() {}, removeItem() {}},
  localStorage: {getItem() { return null; }, setItem() {}},
  matchMedia: () => ({matches: false}),
  Option: class extends Element { constructor(text, value) { super(); this.textContent = text; this.value = value; } },
  document: {
    documentElement: {dataset: {}},
    querySelector(selector) {
      if (selector.startsWith('#') && elements.has(selector.slice(1))) return elements.get(selector.slice(1));
      throw new Error('Unexpected selector: ' + selector);
    },
    querySelectorAll: () => [],
    createElement: () => new Element(),
  },
  fetch: async (url, options) => {
    calls.push({url, options});
    if (url === '/api/admin/alerts') {
      if (failMetadata) throw new Error('local simulated disconnect');
      return {ok: true, status: 200, text: async () => JSON.stringify(metadata)};
    }
    assert.equal(url, '/api/admin/alerts/test');
    assert.equal(options.method, 'POST');
    return {ok: testStatus === 200, status: testStatus, text: async () => JSON.stringify(testResult)};
  },
});
vm.runInContext(script, ctx, {filename: 'admin.html'});
const run = code => vm.runInContext(code, ctx);
const el = id => elements.get(id);

(async () => {
  run('token = "local-ui-test"');
  await run('loadAlertReceivers()');
  assert.equal(el('alert-test').disabled, false);
  assert.match(el('alert-receivers').textContent, /钉钉、企业微信/);
  assert.deepEqual(el('alert-receiver').children.map(x => x.value), ['', 'dingtalk', 'wecom']);
  el('alert-receiver').value = 'dingtalk';
  await el('alert-test').onclick();
  assert.equal(JSON.parse(calls.at(-1).options.body).receiver, 'dingtalk');
  assert.equal(el('alert-results').children.length, 1);
  assert.match(el('alert-results').children[0].textContent, /钉钉：接收端已接受/);
  assert.equal(el('alert-test').disabled, false);
  assert.equal(el('alert-receiver').disabled, false);

  testStatus = 502;
  testResult = {ok: false, error: 'dingtalk: code 310000', results: [
    {receiver: 'dingtalk', name: '钉钉', ok: false, error: 'API rejected notification (code 310000)'},
    {receiver: 'wecom', name: '企业微信', ok: true},
  ]};
  el('alert-receiver').value = '';
  await el('alert-test').onclick();
  assert.equal(el('alert-results').children.length, 2);
  assert.match(el('alert-results').children[0].textContent, /发送失败/);
  assert.match(el('alert-results').children[1].textContent, /接收端已接受/);
  assert.match(el('alert-msg').textContent, /310000/);
  assert.equal(el('alert-refresh').disabled, false);
  assert.equal(el('alert-test').disabled, false);

  run('renderAlertResults([{name:"<img onerror=evil()>",ok:false,error:"<script>bad</script>"}])');
  assert.equal(el('alert-results').children[0].innerHTML, '');
  assert.match(el('alert-results').children[0].textContent, /<script>bad<\/script>/);

  metadata.enabled = false;
  await run('loadAlertReceivers()');
  assert.equal(el('alert-test').disabled, true);
  metadata = {enabled: true, receivers: []};
  await run('loadAlertReceivers()');
  assert.equal(el('alert-test').disabled, true);
  failMetadata = true;
  await run('loadAlertReceivers()');
  assert.match(el('alert-receivers').textContent, /读取失败/);
  assert.equal(el('alert-test').disabled, true);
  run('logout()');
  assert.equal(el('alert-results').children.length, 0);
  assert.equal(el('alert-test').disabled, true);
  console.log('Notification UI checks passed: receiver list, selected test, partial failure, text escaping, disabled state, metadata failure, logout.');
})().catch(error => { console.error(error); process.exitCode = 1; });
