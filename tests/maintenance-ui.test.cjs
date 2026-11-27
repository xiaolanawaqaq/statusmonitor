'use strict';

// Run the actual inline scripts against an in-memory DOM; no browser or dependencies.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

// A non-UTC timezone catches accidental UTC parsing of datetime-local values.
process.env.TZ = 'Asia/Shanghai';
const NOW = Date.parse('2030-01-15T02:00:00Z');
const SECOND = 1000;
const now = NOW / SECOND;
const attack = '<img src=x onerror="evil()"><script>evil()</script>';
const json = value => JSON.parse(JSON.stringify(value));
const tick = () => new Promise(resolve => setImmediate(resolve));
const deferred = () => {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
};
class Clock extends Date {
  constructor(...args) { super(...(args.length ? args : [NOW])); }
  static now() { return NOW; }
}

class Element {
  constructor(tagName = 'div') {
    this.tagName = tagName.toUpperCase();
    this.children = [];
    this.parentElement = null;
    this.attributes = {};
    this.dataset = {};
    this.listeners = {};
    this.className = '';
    this.value = '';
    this.defaultValue = '';
    this.checked = false;
    this.defaultChecked = false;
    this.selected = false;
    this.defaultSelected = false;
    this.disabled = false;
    this.hidden = false;
    this._text = '';
    this._html = '';
    this.classList = {
      add: name => this.toggleClass(name, true),
      remove: name => this.toggleClass(name, false),
      toggle: (name, enabled) => this.toggleClass(name, enabled),
      contains: name => this.className.split(/\s+/).includes(name),
    };
  }
  toggleClass(name, enabled) {
    const names = new Set(this.className.split(/\s+/).filter(Boolean));
    if (enabled === undefined) enabled = !names.has(name);
    if (enabled) names.add(name); else names.delete(name);
    this.className = [...names].join(' ');
    return enabled;
  }
  get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
  set textContent(value) { this._text = String(value ?? ''); this._html = ''; this.detachChildren(); }
  get innerHTML() { return this._html; }
  set innerHTML(value) {
    this._html = String(value); this._text = ''; this.detachChildren();
    // Generated table actions must be real clickable controls in the fixture.
    for (const match of this._html.matchAll(/<button\b([^>]*)>([\s\S]*?)<\/button>/g)) {
      const button = new Element('button');
      applyAttributes(button, match[1]);
      button.textContent = match[2];
      this.append(button);
    }
  }
  detachChildren() { for (const child of this.children) child.parentElement = null; this.children = []; }
  append(...children) {
    for (let child of children) {
      if (typeof child === 'string') { const text = new Element('#text'); text._text = child; child = text; }
      child.parentElement = this;
      this.children.push(child);
    }
  }
  appendChild(child) { this.append(child); return child; }
  replaceChildren(...children) { this.detachChildren(); this._text = ''; this._html = ''; this.append(...children); }
  get options() { return this.tagName === 'SELECT' ? this.children : []; }
  get selectedOptions() { return this.options.filter(option => option.selected); }
  setAttribute(name, value) {
    value = String(value);
    this.attributes[name] = value;
    if (name === 'class') this.className = value;
    else if (name.startsWith('data-')) this.dataset[name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = value;
    else if (['disabled', 'checked', 'selected', 'hidden', 'multiple'].includes(name)) this[name] = true;
    else this[name] = value;
  }
  getAttribute(name) { return name === 'class' ? this.className : this.attributes[name] ?? null; }
  removeAttribute(name) { delete this.attributes[name]; if (name === 'disabled') this.disabled = false; }
  matches(selector) {
    if (selector.startsWith('#')) return this.id === selector.slice(1);
    if (selector.startsWith('.')) return this.classList.contains(selector.slice(1));
    const attribute = /^(\w+)?\[([^=\]]+)(?:=['"]?([^'"\]]+)['"]?)?\]$/.exec(selector);
    if (attribute) {
      const [, tag, name, value] = attribute;
      return (!tag || this.tagName.toLowerCase() === tag) && this.getAttribute(name) != null && (value === undefined || this.getAttribute(name) === value);
    }
    return this.tagName.toLowerCase() === selector;
  }
  querySelectorAll(selector) {
    const parts = selector.trim().split(/\s+/);
    if (parts.length > 1) return this.querySelectorAll(parts[0]).flatMap(element => element.querySelectorAll(parts.slice(1).join(' ')));
    const selectors = selector.split(',');
    return this.children.flatMap(child => [...(selectors.some(part => child.matches(part)) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) { return this.matches(selector) ? this : this.parentElement?.closest(selector) || null; }
  addEventListener(name, handler) { this.listeners[name] = handler; }
  focus() { this.focused = true; }
  scrollIntoView() { this.scrolled = true; }
  reset() {
    this.querySelectorAll('input,select,textarea').forEach(control => {
      control.value = control.defaultValue;
      control.checked = control.defaultChecked;
      control.options.forEach(option => option.selected = option.defaultSelected);
    });
  }
}
function applyAttributes(element, attributes) {
  for (const match of attributes.matchAll(/([\w:-]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/g)) {
    element.setAttribute(match[1], match[2] ?? match[3] ?? match[4] ?? '');
  }
  element.defaultValue = element.value;
  element.defaultChecked = element.checked;
  element.defaultSelected = element.selected;
}
function parseHTML(html) {
  const document = new Element('document');
  const stack = [document];
  const source = html.replace(/<style>[\s\S]*?<\/style>/g, '').replace(/<script>[\s\S]*?<\/script>/g, '');
  for (const match of source.matchAll(/<!--[^]*?-->|<![^>]*>|<\/[\w-]+\s*>|<([\w-]+)\b([^>]*)>|([^<]+)/g)) {
    const token = match[0];
    if (token.startsWith('<!')) continue;
    if (token.startsWith('</')) { stack.pop(); continue; }
    if (match[1]) {
      const element = new Element(match[1]);
      applyAttributes(element, match[2]);
      stack.at(-1).append(element);
      if (!['input', 'meta', 'link', 'br', 'hr', 'img'].includes(match[1])) stack.push(element);
    } else if (match[3]?.trim()) stack.at(-1)._text += match[3];
  }
  document.documentElement = document.querySelector('html');
  document.body = document.querySelector('body');
  document.head = document.querySelector('head');
  document.createElement = tag => new Element(tag);
  return document;
}
const channelsFixture = () => [
  { id: 'mc-1', name: 'Minecraft 主服', group: '游戏', type: 'minecraft', enabled: true, sort: 1 },
  { id: 'steam-2', name: 'Source 备用服', group: '游戏', type: 'steam', enabled: false, sort: 2 },
];
const windowFixture = (overrides = {}) => ({
  id: 123, title: '升级游戏服务器', body: '预计短暂断线', start_at: now - 60, end_at: now + 3600,
  cancelled_at: null, created_at: now - 120, all_channels: false, channel_ids: ['mc-1'], state: 'active',
  ...overrides,
});
const dynamicFixture = (overrides = {}) => ({
  id: 'm123', kind: 'maintenance', source: 'scheduled', title: '升级游戏服务器', body: '预计短暂断线',
  state: 'active', start_at: now - 60, end_at: now + 3600, services: ['Minecraft 主服'], updates: [],
  ...overrides,
});
const channelFixture = (overrides = {}) => ({
  id: 'mc-1', name: 'Minecraft 主服', group: '游戏', type: 'minecraft', current_status: 'ok', maintenance: null,
  availability: 25, avg_latency_ms: 10, probes: 4, last_probe_at: now - 10,
  buckets: [{ ts: now - 300, status: 'down', probes: 3, latency_ms: null }], ...overrides,
});
const snapshotFixture = (overrides = {}) => ({
  site: { title: '游戏服状态', description: '状态说明' }, generated_at: now, bucket_seconds: 300,
  channels: [channelFixture()], maintenance: [], ...overrides,
});

async function boot(page = 'admin', options = {}) {
  const html = fs.readFileSync(path.join(__dirname, `../app/static/${page}.html`), 'utf8');
  const document = parseHTML(html);
  const model = {
    channels: channelsFixture(), maintenance: [], dynamics: [], snapshot: snapshotFixture(),
    calls: [], confirmations: [], confirm: true, failures: new Map(), hold: null,
    stored: options.savedToken || null, signal: null,
  };
  const timers = [];
  const ctx = vm.createContext({
    console, AbortController, Date: Clock, setTimeout, clearTimeout,
    setInterval: fn => { timers.push(fn); return timers.length; },
    document, location: { hash: '#/' },
    window: { addEventListener() {}, scrollTo() {} },
    matchMedia: () => ({ matches: false }),
    localStorage: { getItem: () => null, setItem() {} },
    sessionStorage: {
      getItem: () => model.stored,
      setItem: (_, value) => { model.stored = value; },
      removeItem: () => { model.stored = null; },
    },
    Option: class extends Element {
      constructor(text, value) { super('option'); this.textContent = text; this.value = value; }
    },
    confirm: text => { model.confirmations.push(text); return model.confirm; },
    prompt: () => null,
    fetch: async (url, init = {}) => {
      const method = init.method || 'GET';
      const key = `${method} ${url}`;
      model.calls.push({ url, init });
      if (model.hold?.key === key) {
        model.signal = init.signal;
        await model.hold.promise;
      }
      if (model.failures.has(key)) {
        const failure = model.failures.get(key);
        if (failure instanceof Error) throw failure;
        return { ok: false, status: failure.status || 500, text: async () => JSON.stringify({ error: failure.message }) };
      }
      let body;
      let status = 200;
      if (url === '/api/admin/channels' && method === 'GET') body = model.channels;
      else if (url === '/api/admin/alerts') body = { enabled: false, receivers: [] };
      else if (url === '/api/admin/dynamics' || url === '/api/dynamics') body = model.dynamics;
      else if (url === '/api/status') body = model.snapshot;
      else if (url === '/api/admin/maintenance' && method === 'GET') body = model.maintenance;
      else if (url === '/api/admin/maintenance' && method === 'POST') {
        const payload = JSON.parse(init.body);
        body = windowFixture({ ...payload, id: model.maintenance.length + 200, start_at: payload.start_at || now,
          state: payload.start_at > now ? 'scheduled' : 'active' });
        model.maintenance.push(body);
        status = 201;
      } else if (/^\/api\/admin\/maintenance\/\d+\/cancel$/.test(url) && method === 'POST') {
        const id = Number(url.split('/').at(-2));
        const window = model.maintenance.find(item => item.id === id);
        assert.ok(window, 'cancel targets an existing maintenance window');
        window.cancelled_at = now;
        window.state = 'cancelled';
        body = { ok: true };
      } else throw new Error(`Unexpected request: ${key}`);
      return { ok: true, status, text: async () => JSON.stringify(body) };
    },
  });
  const run = code => vm.runInContext(code, ctx);
  const el = id => document.querySelector(`#${id}`);
  const submit = () => el('maintenance-form').onsubmit({ preventDefault() {} });
  const select = ids => {
    el('maintenance-channels').options.forEach(option => { option.selected = ids.includes(option.value); });
    el('maintenance-channels').onchange();
  };
  const fill = (values = {}) => {
    el('maintenance-title').value = values.title ?? '  游戏更新  ';
    el('maintenance-body').value = values.body ?? '维护说明';
    el('maintenance-start').value = values.start ?? '';
    el('maintenance-end').value = values.end ?? '2030-01-15T11:00';
    el('maintenance-all').checked = values.all ?? false;
    select(values.channels ?? ['mc-1']);
    el('maintenance-all').onchange();
  };
  const clickCancel = id => {
    const button = el('maintenance-list').querySelectorAll('button').find(item => item.dataset.maintenanceCancel === String(id));
    assert.ok(button, `cancel button for ${id}`);
    return { button, click: () => el('maintenance-list').onclick({ target: button }) };
  };
  vm.runInContext(html.match(/<script>([\s\S]*?)<\/script>/)[1], ctx, { filename: `${page}.html` });
  await tick();
  if (page === 'admin' && !options.savedToken && options.login !== false) {
    el('token').value = 'local-maintenance-ui-test';
    await run('login()');
  }
  return { html, document, model, ctx, run, el, fill, select, submit, clickCancel, timers };
}
const maintenancePosts = ui => ui.model.calls.filter(call => call.url === '/api/admin/maintenance' && call.init.method === 'POST');

// Each case exercises actual event handlers/renderers, not a copy of their logic.
test('independent maintenance form, login loading, local time conversion and explicit service scope', async () => {
  const ui = await boot();
  assert.equal(ui.el('panel').classList.contains('hidden'), false);
  assert.ok(ui.model.calls.some(call => call.url === '/api/admin/maintenance'));
  assert.equal(ui.el('maintenance-form').closest('#maintenance-section').id, 'maintenance-section');
  assert.equal(ui.el('maintenance-form').closest('#dyn-form'), null);
  assert.equal(ui.el('maintenance-start').type, 'datetime-local');
  assert.equal(ui.el('maintenance-end').getAttribute('required'), '');
  assert.equal(ui.el('maintenance-channels').multiple, true);
  assert.match(ui.el('maintenance-channels').options[1].textContent, /已停用/);
  ui.fill({ start: '2030-01-15T10:30', end: '2030-01-15T12:30', channels: ['mc-1', 'steam-2'] });
  assert.match(ui.el('maintenance-scope-summary').textContent, /已选 2 个渠道/);
  await ui.submit();
  const call = maintenancePosts(ui)[0];
  assert.deepEqual(JSON.parse(call.init.body), {
    title: '游戏更新', body: '维护说明', start_at: 1894674600, end_at: 1894681800,
    all_channels: false, channel_ids: ['mc-1', 'steam-2'],
  });
  assert.equal(call.init.headers['X-Admin-Token'], 'local-maintenance-ui-test');
  assert.equal(call.init.headers['Content-Type'], 'application/json');
  assert.match(ui.el('maintenance-msg').textContent, /维护计划已创建/);
  assert.match(ui.el('maintenance-list').innerHTML, /待开始|取消计划/);
  assert.match(ui.el('maintenance-list').innerHTML, /Minecraft 主服 \(mc-1\).*Source 备用服 \(steam-2\)/);
  assert.equal(ui.el('maintenance-title').value, '');
  assert.equal(ui.el('maintenance-save').disabled, false);
  assert.equal(ui.el('maintenance-form').getAttribute('aria-busy'), null);

  ui.fill({ all: true });
  assert.equal(ui.el('maintenance-channels').disabled, true);
  assert.match(ui.el('maintenance-scope-summary').textContent, /所有服务.*新增渠道/);
  await ui.submit();
  const immediate = JSON.parse(maintenancePosts(ui)[1].init.body);
  assert.equal(immediate.start_at, 0);
  assert.equal(immediate.all_channels, true);
  assert.deepEqual(immediate.channel_ids, []);
  assert.match(ui.el('maintenance-msg').textContent, /维护窗口已生效/);
  assert.match(ui.el('maintenance-list').innerHTML, /所有服务（包括新增渠道）/);
  assert.match(ui.el('maintenance-list').innerHTML, /提前结束/);
  assert.equal(ui.el('maintenance-channels').disabled, false);
});

test('scope and time validation rejects invalid submissions and allows the 30-day boundary', async () => {
  const ui = await boot();
  const cases = [
    [{ title: ' ' }, /标题不能为空/],
    [{ channels: [] }, /至少一个有效渠道/],
    [{ end: '' }, /结束时间必填/],
    [{ end: 'not-a-date' }, /有效的本地时间/],
    [{ start: '2030-02-30T10:00', end: '2030-03-01T12:00' }, /有效的本地时间/],
    [{ start: '2030-01-15T12:00', end: '2030-01-15T11:00' }, /晚于/],
    [{ end: '2030-01-15T09:59' }, /晚于/],
    [{ start: '2030-01-15T10:00', end: '2030-02-14T10:01' }, /不能超过 30 天/],
    [{ end: '2030-02-14T10:01' }, /不能超过 30 天/],
  ];
  for (const [values, message] of cases) {
    ui.fill(values);
    await ui.submit();
    assert.match(ui.el('maintenance-msg').textContent, message);
    assert.equal(maintenancePosts(ui).length, 0);
  }
  ui.fill();
  ui.el('maintenance-channels').options[0].value = 'removed-channel';
  await ui.submit();
  assert.match(ui.el('maintenance-msg').textContent, /有效渠道/);
  assert.equal(maintenancePosts(ui).length, 0);
  ui.run('renderMaintenanceChannels()');
  ui.fill({ start: '2030-01-15T10:00', end: '2030-02-14T10:00' });
  await ui.submit();
  assert.equal(maintenancePosts(ui).length, 1);
  const payload = JSON.parse(maintenancePosts(ui)[0].init.body);
  assert.equal(payload.end_at - payload.start_at, 30 * 86400);
});

test('create failure is visible, preserves input, prevents duplicate submit and restores controls', async () => {
  const ui = await boot();
  ui.fill({ all: true });
  const gate = deferred();
  ui.model.hold = { key: 'POST /api/admin/maintenance', ...gate };
  ui.model.failures.set('POST /api/admin/maintenance', { status: 400, message: `维护创建失败 ${attack}` });
  const submitting = ui.submit();
  assert.equal(ui.el('maintenance-save').disabled, true);
  assert.equal(ui.el('maintenance-title').disabled, true);
  assert.equal(ui.el('maintenance-refresh').disabled, true);
  assert.equal(ui.el('maintenance-form').getAttribute('aria-busy'), 'true');
  await ui.submit();
  assert.equal(maintenancePosts(ui).length, 1);
  gate.resolve();
  await submitting;
  assert.match(ui.el('maintenance-msg').textContent, /维护创建失败/);
  assert.ok(ui.el('maintenance-msg').textContent.includes(attack));
  assert.equal(ui.el('maintenance-msg').innerHTML, '');
  assert.equal(ui.el('maintenance-title').value, '  游戏更新  ');
  assert.equal(ui.el('maintenance-save').disabled, false);
  assert.equal(ui.el('maintenance-refresh').disabled, false);
  assert.equal(ui.el('maintenance-channels').disabled, true, 'all-services scope stays selected after failure');
  ui.model.hold = null;
  ui.model.failures.delete('POST /api/admin/maintenance');
  await ui.submit();
  assert.match(ui.el('maintenance-msg').textContent, /已生效/);
});

test('only unexpired windows can be cancelled, with confirmation, busy state and synchronized lists', async () => {
  const ui = await boot();
  ui.model.maintenance = [
    windowFixture(),
    windowFixture({ id: 124, state: 'scheduled', start_at: now + 600 }),
    windowFixture({ id: 125, state: 'completed', end_at: now - 1 }),
    windowFixture({ id: 126, state: 'cancelled', cancelled_at: now - 1 }),
    windowFixture({ id: 127, state: 'active', end_at: now - 1 }),
  ];
  await ui.run('loadMaintenance()');
  assert.deepEqual(ui.el('maintenance-list').querySelectorAll('button').map(button => button.dataset.maintenanceCancel), ['123', '124']);
  assert.equal(ui.clickCancel(123).button.textContent, '提前结束');
  assert.equal(ui.clickCancel(124).button.textContent, '取消计划');
  ui.model.confirm = false;
  await ui.clickCancel(124).click();
  assert.equal(ui.model.calls.filter(call => call.url.endsWith('/cancel')).length, 0);
  assert.match(ui.model.confirmations.at(-1), /取消计划.*\n范围：Minecraft 主服/);
  ui.model.confirm = true;
  const gate = deferred();
  ui.model.hold = { key: 'POST /api/admin/maintenance/123/cancel', ...gate };
  const { button, click } = ui.clickCancel(123);
  const cancelling = click();
  assert.equal(button.disabled, true);
  assert.equal(button.textContent, '处理中…');
  assert.equal(ui.el('maintenance-save').disabled, true);
  await click();
  assert.equal(ui.model.calls.filter(call => call.url.endsWith('/cancel')).length, 1);
  gate.resolve();
  await cancelling;
  assert.match(ui.el('maintenance-msg').textContent, /已提前结束/);
  assert.equal(ui.el('maintenance-save').disabled, false);
  assert.equal(ui.el('maintenance-list').querySelectorAll('button').length, 1);
  const lastURLs = ui.model.calls.slice(-2).map(call => call.url);
  assert.deepEqual(new Set(lastURLs), new Set(['/api/admin/maintenance', '/api/admin/dynamics']));

  ui.model.hold = null;
  ui.model.failures.set('POST /api/admin/maintenance/124/cancel', { status: 409, message: '维护已结束，请刷新' });
  await ui.clickCancel(124).click();
  assert.match(ui.el('maintenance-msg').textContent, /维护已结束，请刷新/);
  assert.equal(ui.clickCancel(124).button.disabled, false);
  ui.model.failures.delete('POST /api/admin/maintenance/124/cancel');
  await ui.clickCancel(124).click();
  assert.match(ui.el('maintenance-msg').textContent, /计划已取消/);
  assert.equal(ui.el('maintenance-list').querySelectorAll('button').length, 0);
  const stale = new Element('button');
  stale.setAttribute('data-maintenance-cancel', '127');
  const before = ui.model.calls.length;
  await ui.el('maintenance-list').onclick({ target: stale });
  assert.equal(ui.model.calls.length, before);
  assert.match(ui.el('maintenance-msg').textContent, /已结束或取消/);
});

test('load errors retain data, refresh updates channel choices and retry recovers', async () => {
  const ui = await boot();
  ui.model.maintenance = [windowFixture()];
  await ui.run('loadMaintenance()');
  const previous = ui.el('maintenance-list').innerHTML;
  ui.model.failures.set('GET /api/admin/maintenance', new Error(`网络断开 ${attack}`));
  await ui.el('maintenance-refresh').onclick();
  assert.equal(ui.el('maintenance-error').hidden, false);
  assert.match(ui.el('maintenance-error').querySelector('span').textContent, /网络断开/);
  assert.equal(ui.el('maintenance-error').querySelector('span').innerHTML, '');
  // The fixture keeps the assigned HTML; control properties below reflect later changes.
  assert.equal(ui.el('maintenance-list').innerHTML.replace(/ disabled/g, ''), previous);
  assert.equal(ui.clickCancel(123).button.disabled, false);
  assert.equal(ui.el('maintenance-refresh').disabled, false);
  assert.equal(ui.el('maintenance-retry').disabled, false);
  assert.equal(ui.el('maintenance-list').getAttribute('aria-busy'), 'false');
  ui.model.failures.delete('GET /api/admin/maintenance');
  await ui.el('maintenance-retry').onclick();
  assert.equal(ui.el('maintenance-error').hidden, true);
  ui.fill({ channels: ['mc-1', 'steam-2'] });
  ui.model.channels = [{ ...ui.model.channels[0], name: '新名称' }];
  const before = ui.model.calls.length;
  await ui.el('panel-refresh').onclick();
  assert.deepEqual(new Set(ui.model.calls.slice(before).map(call => call.url)), new Set([
    '/api/admin/channels', '/api/admin/dynamics', '/api/admin/alerts', '/api/admin/maintenance',
  ]));
  assert.deepEqual(ui.el('maintenance-channels').selectedOptions.map(option => option.value), ['mc-1']);
  assert.match(ui.el('maintenance-scope-summary').textContent, /新名称/);
  assert.match(ui.el('maintenance-list').innerHTML, /新名称/);
});

test('scheduled dynamics remain read-only while existing human dynamics stay editable', async () => {
  const ui = await boot();
  ui.model.dynamics = [
    dynamicFixture({ state: 'pending' }),
    dynamicFixture({ id: 'd123', source: 'human', kind: 'notice', state: 'published', title: '普通公告' }),
  ];
  await ui.run('loadDynamics()');
  assert.match(ui.el('dyn-list').innerHTML, /维护记录只读，请到维护窗口区操作/);
  assert.match(ui.el('dyn-list').innerHTML, /待开始/);
  assert.doesNotMatch(ui.el('dyn-list').innerHTML, /data-dyn-\w+="m123"/);
  assert.match(ui.el('dyn-list').innerHTML, /data-dyn-edit="d123"/);
  const fake = new Element('button');
  fake.setAttribute('data-dyn-edit', 'm123');
  const before = ui.model.calls.length;
  await ui.el('dyn-list').onclick({ target: fake });
  assert.equal(ui.model.calls.length, before);
  assert.match(ui.el('dyn-msg').textContent, /只读/);
  assert.equal(ui.el('maintenance-section').scrolled, true);
});

test('logout and expired login clear maintenance content and ignore late responses', async () => {
  const ui = await boot('admin', { savedToken: 'saved-ui-token' });
  assert.equal(ui.el('panel').classList.contains('hidden'), false);
  ui.fill();
  const gate = deferred();
  ui.model.hold = { key: 'POST /api/admin/maintenance', ...gate };
  const submitting = ui.submit();
  ui.el('logout-btn').onclick();
  assert.equal(ui.model.signal.aborted, true);
  assert.equal(ui.model.stored, null);
  assert.equal(ui.el('maintenance-list').innerHTML, '');
  assert.equal(ui.el('maintenance-title').value, '');
  assert.equal(ui.el('maintenance-msg').textContent, '');
  assert.equal(ui.el('maintenance-channels').options.length, 0);
  assert.equal(ui.el('maintenance-form').getAttribute('aria-busy'), null);
  assert.equal(ui.el('maintenance-save').disabled, false);
  gate.resolve();
  await submitting;
  assert.equal(ui.el('maintenance-list').innerHTML, '');
  assert.equal(ui.el('maintenance-msg').textContent, '');
  assert.equal(ui.el('panel').classList.contains('hidden'), true);

  ui.model.hold = null;
  ui.el('token').value = 'new-ui-token';
  await ui.run('login()');
  assert.equal(ui.el('panel').classList.contains('hidden'), false);
  assert.equal(ui.el('maintenance-channels').options.length, 2);
  ui.model.failures.set('GET /api/admin/maintenance', { status: 401, message: 'token expired' });
  await ui.run('loadMaintenance()');
  assert.match(ui.el('login-msg').textContent, /登录已失效.*token expired/);
  assert.equal(ui.el('maintenance-list').innerHTML, '');
  assert.equal(ui.el('maintenance-error').hidden, true);
  assert.equal(ui.el('maintenance-retry').disabled, false);
});

test('maintenance text, channel labels, bodies and scope are escaped in admin rendering', async () => {
  const ui = await boot();
  ui.model.channels = [{ ...channelsFixture()[0], name: attack, id: attack }];
  ui.model.maintenance = [windowFixture({ title: attack, body: attack, channel_ids: [attack] })];
  ui.model.dynamics = [dynamicFixture({ title: attack, body: attack })];
  await ui.run('refreshPanel()');
  assert.equal(ui.el('maintenance-channels').options[0].textContent, `${attack} (${attack})`);
  assert.equal(ui.el('maintenance-channels').options[0].innerHTML, '');
  for (const id of ['maintenance-list', 'dyn-list']) {
    assert.doesNotMatch(ui.el(id).innerHTML, /<img|<script>/);
    assert.match(ui.el(id).innerHTML, /&lt;img/);
    assert.match(ui.el(id).innerHTML, /&quot;evil\(\)&quot;/);
  }
});

test('maintenance has blue identity and independent banners while raw failure history stays visible', async () => {
  const ui = await boot('index');
  const active = windowFixture({ title: attack, body: attack });
  const scheduled = windowFixture({ id: 124, state: 'scheduled', start_at: now + 600, all_channels: true });
  ui.model.snapshot = snapshotFixture({
    maintenance: [active, scheduled],
    channels: [channelFixture({ maintenance: active, current_status: 'partial' }), channelFixture({ id: 'web', name: '网站' })],
  });
  ui.model.dynamics = [dynamicFixture({ id: 'd1', source: 'human', kind: 'notice', state: 'published', title: '普通公告' })];
  const original = json(ui.model.snapshot);
  await ui.run('loadStatus()');
  await ui.run('loadDynamics()');
  assert.equal(ui.run('currentStatus(state.data.channels[0])'), 'partial');
  assert.equal(ui.el('overall-title').textContent, '服务维护中');
  assert.equal(ui.el('overall').className, 'overall maintenance');
  assert.match(ui.el('overall-desc').textContent, /维护中：Minecraft 主服/);
  assert.doesNotMatch(ui.el('overall-desc').textContent, /部分失败|故障/);
  assert.match(ui.el('groups').innerHTML, /icon maintenance/);
  assert.match(ui.el('groups').innerHTML, /svg class="wrench"/);
  assert.match(ui.el('groups').innerHTML, /当前：维护中/);
  assert.match(ui.el('groups').innerHTML, /25\.00% 可用/);
  assert.equal((ui.el('groups').innerHTML.match(/class="bar down"/g) || []).length, 2);
  assert.deepEqual(json(ui.run('state.data.channels[0].buckets')), original.channels[0].buckets);
  assert.equal(ui.el('maintenance-banners').hidden, false);
  assert.equal(ui.el('maintenance-banners').children.length, 2);
  const [activeBanner, scheduledBanner] = ui.el('maintenance-banners').children;
  assert.equal(activeBanner.href, '#/history/m123');
  assert.match(activeBanner.textContent, /维护中.*范围：Minecraft 主服/);
  assert.ok(activeBanner.textContent.includes(attack));
  assert.equal(activeBanner.querySelector('.banner-title').innerHTML, '');
  assert.equal(activeBanner.querySelector('.banner-body').innerHTML, '');
  assert.match(scheduledBanner.textContent, /维护预告/);
  assert.ok(scheduledBanner.textContent.includes(ui.run(`fmtTime(${scheduled.start_at})`)));
  assert.ok(scheduledBanner.textContent.includes(ui.run(`fmtTime(${scheduled.end_at})`)));
  assert.match(scheduledBanner.textContent, /范围：所有服务/);
  assert.equal(ui.el('banners').children.length, 1);
  assert.match(ui.el('banners').textContent, /普通公告/);
  assert.doesNotMatch(ui.el('groups').innerHTML, /<img|<script>/);
  assert.match(ui.el('groups').innerHTML, /&lt;img/);
  assert.match(ui.html, /\.group-head \.icon\.maintenance\{[^}]*var\(--blue/);

  ui.model.snapshot.channels[0].current_status = 'down';
  await ui.run('loadStatus()');
  assert.equal(ui.el('overall-title').textContent, '服务维护中', 'raw maintenance failure cannot make the overall state an outage');
  ui.model.snapshot.channels[1].current_status = 'down';
  await ui.run('loadStatus()');
  assert.equal(ui.el('overall').className, 'overall outage');
  assert.match(ui.el('overall-title').textContent, /服务故障/);
  assert.match(ui.el('overall-desc').textContent, /故障：网站/);
  assert.doesNotMatch(ui.el('overall-desc').textContent, /故障：Minecraft/);
  ui.model.snapshot.channels[1].current_status = 'nodata';
  await ui.run('loadStatus()');
  assert.equal(ui.el('overall').className, 'overall degraded');
  assert.match(ui.el('overall-desc').textContent, /无数据：网站/);
});

test('scheduled windows do not affect overall state and clearing windows clears maintenance banners', async () => {
  const ui = await boot('index');
  ui.model.snapshot.maintenance = [windowFixture({ state: 'scheduled', start_at: now + 600 })];
  await ui.run('loadStatus()');
  assert.match(ui.el('overall-title').textContent, /所有服务运行正常/);
  assert.equal(ui.el('maintenance-banners').children.length, 1);
  ui.model.snapshot.maintenance = [windowFixture({ state: 'completed' }), windowFixture({ id: 125, state: 'cancelled', cancelled_at: now })];
  await ui.run('loadStatus()');
  assert.equal(ui.el('maintenance-banners').hidden, true);
  assert.equal(ui.el('maintenance-banners').children.length, 0);
  delete ui.model.snapshot.maintenance;
  delete ui.model.snapshot.channels[0].maintenance;
  await ui.run('loadStatus()');
  assert.match(ui.el('overall-title').textContent, /所有服务运行正常/);
  ui.model.snapshot.channels = [];
  await ui.run('loadStatus()');
  assert.equal(ui.el('overall').className, 'overall unknown');
});

test('maintenance and pending history filters, string ID routing and detail XSS safety', async () => {
  const ui = await boot('index');
  ui.model.dynamics = [
    dynamicFixture({ state: 'pending', title: attack, body: attack, services: [attack], updates: [{ at: now, label: attack, body: attack }] }),
    dynamicFixture({ id: 'm124', state: 'active', title: '正在维护' }),
    dynamicFixture({ id: 'm125', state: 'resolved', title: '维护已结束' }),
    dynamicFixture({ id: 'd1', source: 'human', kind: 'notice', state: 'published', title: '既有公告' }),
    dynamicFixture({ id: 'i1', source: 'auto', kind: 'incident', state: 'active', title: '既有故障' }),
  ];
  await ui.run('loadDynamics()');
  ui.run('location.hash = "#/history"; route()');
  const filter = value => {
    const button = ui.el('filters').querySelectorAll('button').find(item => item.dataset.filter === value);
    assert.ok(button, `filter ${value} exists`);
    ui.el('filters').listeners.click({ target: button });
    assert.equal(button.getAttribute('aria-pressed'), 'true');
  };
  filter('maintenance');
  assert.equal(ui.el('historyCount').textContent, '共 3 条');
  assert.match(ui.el('incidents').innerHTML, /incident-card pending maintenance/);
  assert.match(ui.el('incidents').innerHTML, /待开始|维护中|已结束/);
  assert.doesNotMatch(ui.el('incidents').innerHTML, /既有公告|既有故障|<img|<script>/);
  assert.match(ui.el('incidents').innerHTML, /范围：&lt;img/);
  filter('pending');
  assert.equal(ui.el('historyCount').textContent, '共 1 条');
  filter('incident');
  assert.equal(ui.el('historyCount').textContent, '共 1 条');
  assert.match(ui.el('incidents').innerHTML, /既有故障/);
  filter('notice');
  assert.match(ui.el('incidents').innerHTML, /既有公告/);
  filter('resolved');
  assert.match(ui.el('incidents').innerHTML, /维护已结束/);
  ui.run('location.hash = "#/history/%6D123"; route()');
  assert.equal(ui.run('state.dynamicID'), 'm123');
  assert.equal(ui.el('history-detail').hidden, false);
  assert.match(ui.el('history-detail').innerHTML, /待开始/);
  assert.match(ui.el('history-detail').innerHTML, /&lt;img/);
  assert.doesNotMatch(ui.el('history-detail').innerHTML, /<img|<script>/);
  assert.match(ui.el('history-detail').innerHTML, /影响：&lt;img/);
  assert.match(ui.el('history-detail').innerHTML, /更新记录/);
});

test('status refresh updates independent maintenance data and surfaces failures without deleting prior banners', async () => {
  const ui = await boot('index');
  ui.model.snapshot.maintenance = [windowFixture()];
  await ui.run('loadStatus()');
  ui.model.failures.set('GET /api/status', { message: `维护状态获取失败 ${attack}` });
  await ui.run('loadStatus()');
  assert.equal(ui.el('status-error').hidden, false);
  assert.match(ui.el('status-error').querySelector('span').textContent, /维护状态获取失败/);
  assert.equal(ui.el('status-error').querySelector('span').innerHTML, '');
  assert.match(ui.el('overall-title').textContent, /暂时无法获取/);
  assert.equal(ui.el('maintenance-banners').children.length, 1);
  ui.model.failures.clear();
  ui.model.snapshot.maintenance = [];
  ui.timers[0]();
  await tick();
  assert.equal(ui.el('status-error').hidden, true);
  assert.equal(ui.el('maintenance-banners').children.length, 0);
});

test('channel list surfaces configuration errors and probe failures', async () => {
  const ui = await boot();
  ui.model.channels = [
    { id: 'needs-key', name: 'GPT', group: 'AI', type: 'openai', enabled: true, sort: 1,
      current_status: 'config_error', config_error: true, last_error: 'missing API key' },
    { id: 'real-down', name: 'Game', group: '游戏', type: 'tcp', enabled: true, sort: 2,
      current_status: 'down', config_error: false, last_error: 'connection refused' },
    { id: 'healthy', name: 'Site', group: '站点', type: 'http', enabled: true, sort: 3,
      current_status: 'ok', config_error: false },
    { id: 'stopped', name: 'Old', group: '站点', type: 'http', enabled: false, sort: 4,
      current_status: 'nodata' },
  ];
  ui.ctx.__list = ui.model.channels;
  ui.run('renderChannels(__list)');
  const html = ui.el('channels').innerHTML;
  assert.match(html, /配置错误/);
  assert.match(html, /missing API key/);
  assert.match(html, /探测失败/);
  assert.match(html, /探测正常/);
  assert.match(html, /无数据/);
  // A disabled channel keeps its "停用" badge and is not shown as healthy.
  assert.match(html, /停用/);
});
