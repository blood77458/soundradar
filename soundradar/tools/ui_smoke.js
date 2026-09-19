/*
 * soundradar 实时打分面板 UI 冒烟测试（纯 node，无需浏览器）。
 *
 * 目的：在没有浏览器的环境里，用最小 DOM 桩真实执行 internal/server/static/app.js，
 * 验证「实时打分」页签的接线是对的：
 *   1. index.html 里存在 app.js 引用的每一个 id（防止改了 HTML 忘了改 JS）；
 *   2. 设备下拉 / 阈值下拉能按 API 返回填充；
 *   3. renderTick / renderBars / renderStats / pushEvent 真的能把数据写进 DOM
 *      （电平条宽度、峰值保持、分数条宽度与配色、事件时间线、统计行）；
 *   4. SSE 客户端回调（hello/tick/event/state）能正确处理后端推送的 JSON；
 *   5. 阈值滑块走 PATCH /api/items/{id}。
 *
 * 用法: node tools/ui_smoke.js [static 目录]
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const staticDir = process.argv[2] || path.join(__dirname, '..', 'internal', 'server', 'static');
const js = fs.readFileSync(path.join(staticDir, 'app.js'), 'utf8');
const html = fs.readFileSync(path.join(staticDir, 'index.html'), 'utf8');
const css = fs.readFileSync(path.join(staticDir, 'style.css'), 'utf8');

const failures = [];
let checks = 0;
function check(name, cond, detail) {
  checks++;
  if (cond) {
    console.log(`  [PASS] ${name}${detail ? ' - ' + detail : ''}`);
  } else {
    failures.push(`${name}${detail ? ' (' + detail + ')' : ''}`);
    console.log(`  [FAIL] ${name}${detail ? ' - ' + detail : ''}`);
  }
}

// ---------------------------------------------------------------------------
// 最小 DOM 桩
// ---------------------------------------------------------------------------
class El {
  constructor(id, tag) {
    this.id = id || '';
    this.tagName = (tag || 'div').toUpperCase();
    this.className = '';
    this.children = [];
    this._text = '';
    this._html = '';
    this.style = {};
    this.dataset = {};
    this.classList = {
      _s: new Set(),
      add: (c) => this.classList._s.add(c),
      remove: (c) => this.classList._s.delete(c),
      contains: (c) => this.classList._s.has(c) || String(this.className).split(/\s+/).indexOf(c) >= 0,
      toggle: (c, on) => {
        if (on === undefined) { this.classList._s.has(c) ? this.classList._s.delete(c) : this.classList._s.add(c); }
        else if (on) { this.classList._s.add(c); } else { this.classList._s.delete(c); }
      },
    };
    this.value = '';
    this.title = '';
    this.disabled = false;
    this.files = [];
  }
  get textContent() { return this._text; }
  set textContent(v) { this._text = String(v); }
  get innerHTML() { return this._html; }
  set innerHTML(v) {
    this._html = String(v);
    this.children = [];
    if (this._html.indexOf('<') < 0) return;
    // 极简 HTML 解析：只需要理解 app.js 写出来的那点标签汤。
    const stack = [this];
    const re = /<(\/?)([a-zA-Z0-9]+)([^>]*?)(\/?)>/g;
    let m;
    while ((m = re.exec(this._html)) !== null) {
      if (m[1] === '/') { if (stack.length > 1) stack.pop(); continue; }
      const el = new El('', m[2]);
      const cm = /class="([^"]*)"/.exec(m[3]);
      if (cm) el.className = cm[1];
      stack[stack.length - 1].children.push(el);
      if (m[4] !== '/') stack.push(el);
    }
  }
  appendChild(c) { this.children.push(c); return c; }
  querySelector(sel) {
    const want = String(sel).replace(/^\./, '');
    let found = null;
    (function walk(node) {
      for (const c of node.children) {
        if (found) return;
        if (String(c.className).split(/\s+/).indexOf(want) >= 0) { found = c; return; }
        walk(c);
      }
    })(this);
    return found;
  }
  addEventListener() {}
  remove() {}
  click() {}
}

function textOf(node, out) {
  out = out || [];
  if (node._text) out.push(node._text);
  node.children.forEach((c) => textOf(c, out));
  return out;
}

const ids = new Set();
for (const m of html.matchAll(/id="([^"]+)"/g)) ids.add(m[1]);
const byId = new Map();
function getEl(id) {
  if (!byId.has(id)) byId.set(id, new El(id));
  return byId.get(id);
}

class EventSourceStub {
  constructor(url) { this.url = url; this.listeners = {}; EventSourceStub.last = this; }
  addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); }
  close() { this.closed = true; }
  emit(ev, data) { (this.listeners[ev] || []).forEach((fn) => fn({ data: JSON.stringify(data) })); }
}

const apiCalls = [];
const libraryJSON = {
  name: '测试库', schema: 1, createdAt: new Date().toISOString(), path: 'x.srz',
  itemCount: 2, sampleCount: 2, fileBytes: 4096, feature: {}, warnings: [],
  items: [
    { id: 'aaaaaaaa', name: '880Hz 测试音', threshold: 0.75, cooldownMs: 400, tags: [], note: '', sampleCount: 1, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() },
    { id: 'bbbbbbbb', name: '1500Hz 测试音', threshold: 0.8, cooldownMs: 400, tags: [], note: '', sampleCount: 1, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() },
  ],
};

const sandbox = {
  document: {
    getElementById: getEl,
    createElement: (tag) => new El('', tag),
    addEventListener: () => {},
  },
  window: { addEventListener: () => {}, scrollTo: () => {} },
  location: { hash: '#/live' },
  EventSource: EventSourceStub,
  confirm: () => true,
  setTimeout, clearTimeout, console, Date, Math, Number, String, JSON,
  isNaN, isFinite, parseInt, parseFloat, Set, Map, Array, Object, Error, RegExp, Promise,
  URL: { createObjectURL: () => 'blob:x' },
  fetch: async (url, opts) => {
    const method = (opts && opts.method) || 'GET';
    apiCalls.push({ url, method });
    let body;
    if (url.indexOf('/api/library') >= 0) {
      body = libraryJSON;
    } else if (url.indexOf('/api/live/devices') >= 0) {
      body = { devices: [{ index: 0, name: '扬声器', id: '{0}', default: true, defaultComms: true }] };
    } else if (url.indexOf('/api/items/') >= 0 && method === 'PATCH') {
      // PATCH /api/items/{id} 返回更新后的条目（和真实服务端一致）
      const id = url.split('/').pop();
      const it = Object.assign({}, libraryJSON.items.find((x) => x.id === id) || libraryJSON.items[0]);
      try { Object.assign(it, JSON.parse(opts.body)); } catch (e) { /* ignore */ }
      body = it;
    } else {
      body = { running: false };
    }
    return { ok: true, status: 200, text: async () => JSON.stringify(body), json: async () => body };
  },
};
sandbox.globalThis = sandbox;
vm.createContext(sandbox);

let loadErr = null;
try {
  vm.runInContext(js, sandbox, { filename: 'app.js' });
} catch (e) {
  loadErr = e;
}

// app.js 顶层用的是 const/let（词法作用域），要用同一 context 里的取数脚本拿到引用。
const api = loadErr ? null : vm.runInContext(
  '({live, renderTick, renderStats, pushEvent, connectSSE, applyThreshold, loadLibrary, loadLive, dbfsPct, scoreColor, \$})',
  sandbox, { filename: 'api.js' });

async function main() {
  check('app.js 在 DOM 桩下能加载执行', loadErr === null, loadErr ? String(loadErr) : '');
  if (loadErr) { return finish(); }

  // -------------------------------------------------------------------------
  // 1. HTML id 与 JS 引用的一致性
  // -------------------------------------------------------------------------
  const refs = new Set();
  for (const m of js.matchAll(/\$\('([A-Za-z0-9_]+)'\)/g)) refs.add(m[1]);
  const missing = [...refs].filter((id) => !ids.has(id));
  check('index.html 含 app.js 引用的全部 id', missing.length === 0,
    missing.length ? '缺少: ' + missing.join(', ') : `${refs.size} 个 id 全部存在`);

  const liveIds = ['viewLive', 'liveDevice', 'liveWav', 'liveTickMs', 'liveTopN', 'btnLiveStart', 'btnLiveStop',
    'liveSource', 'liveError', 'levelMeter', 'levelFill', 'levelPeak', 'levelGate', 'levelText', 'liveStats',
    'liveWarn', 'topBars', 'eventCount', 'eventList', 'thrItem', 'thrValue', 'thrText', 'btnThrApply', 'thrStatus',
    'tabLib', 'tabLive'];
  const missLive = liveIds.filter((id) => !ids.has(id));
  check('实时打分面板的控件齐全', missLive.length === 0,
    missLive.length ? '缺少: ' + missLive.join(', ') : `${liveIds.length} 个`);

  check('index.html 有「实时打分」页签', html.indexOf('实时打分') >= 0);
  check('style.css 含面板样式（meter/bars/events/tabs）',
    css.indexOf('.meterFill') >= 0 && css.indexOf('.barRow') >= 0 && css.indexOf('ul.events') >= 0 && css.indexOf('.tab.active') >= 0);
  check('前端没有引入任何 CDN / 框架', !/<script[^>]+src="https?:/.test(html) && html.indexOf('cdn.') < 0);

  // -------------------------------------------------------------------------
  // 2. 进入实时页签（设备下拉 / 阈值下拉）
  // -------------------------------------------------------------------------
  await api.loadLive();
  check('loadLive 拉取 /api/library 与 /api/live/devices、/api/live',
    apiCalls.some((c) => c.url === '/api/library') && apiCalls.some((c) => c.url === '/api/live/devices') && apiCalls.some((c) => c.url === '/api/live'));
  check('设备下拉含「默认端点」+ 枚举到的端点',
    getEl('liveDevice').children.length === 2, `${getEl('liveDevice').children.length} 项`);
  check('阈值下拉填充条目的当前阈值',
    getEl('thrItem').children.length === 2 && getEl('thrText').textContent === '0.75',
    `${getEl('thrItem').children.length} 项 / 当前 ${getEl('thrText').textContent}`);

  // -------------------------------------------------------------------------
  // 3. 渲染函数真的往 DOM 里写数据
  // -------------------------------------------------------------------------
  const topBars = getEl('topBars');
  const stats = { running: true, blocks: 100, windows: 900, ticks: 20, events: 1, silentWindows: 3, samples: 96000, audioSeconds: 2, dropped: 0, elapsedMs: 1000, avgBlockMs: 0.2, maxBlockMs: 1.5, msPer20msAudio: 0.2, heapInuseBytes: 3 * 1024 * 1024, budgetMs: 10 };
  const tick = {
    t: new Date().toISOString(), audioMs: 1234.5, level: -18.0, silent: false, event: null, stats,
    top: [
      { id: 'aaaaaaaa', name: '880Hz 测试音', score: 0.9123, templateIndex: 0 },
      { id: 'bbbbbbbb', name: '1500Hz 测试音', score: 0.4321, templateIndex: 1 },
    ],
  };

  api.renderTick(tick);
  check('renderTick 写电平文本', getEl('levelText').textContent.indexOf('-18.0') >= 0, getEl('levelText').textContent);
  check('renderTick 设电平条宽度 (-18 dBFS -> 70%)', getEl('levelFill').style.width === '70%', getEl('levelFill').style.width);
  check('renderTick 设峰值标记位置', /%$/.test(getEl('levelPeak').style.left), getEl('levelPeak').style.left);
  check('renderTick 设静音门限刻度', /%$/.test(getEl('levelGate').style.left), getEl('levelGate').style.left);
  check('renderTick 生成 top-N 分数条', topBars.children.length === 2, `${topBars.children.length} 行`);
  if (topBars.children.length === 2) {
    const first = topBars.children[0];
    const name = first.querySelector('.barName');
    const fill = first.querySelector('.barFill');
    const sc = first.querySelector('.barScore');
    check('第一名名字带排名', name && name.textContent.indexOf('#1 880Hz') === 0, name && name.textContent);
    check('第一名分数条宽度 = 分数', fill && fill.style.width === '91.23%', fill && fill.style.width);
    check('分数条按分数着色（hsl）', fill && /^hsl\(/.test(fill.style.background), fill && fill.style.background);
    check('第一名显示 4 位小数', sc && sc.textContent === '0.9123', sc && sc.textContent);
    check('第一名标记为 lead', first.classList.contains('lead'));
    check('第二名分数条更短', topBars.children[1].querySelector('.barFill').style.width === '43.21%');
  }
  check('renderStats 写累计统计', getEl('liveStats').textContent.indexOf('块 100') >= 0 && getEl('liveStats').textContent.indexOf('每 20 ms 音频') >= 0,
    getEl('liveStats').textContent.slice(0, 80));
  check('正常时无告警行', getEl('liveWarn').textContent === '', getEl('liveWarn').textContent);

  api.renderStats(Object.assign({}, stats, { dropped: 7, msPer20msAudio: 12.5 }));
  check('丢帧/超预算出现告警', getEl('liveWarn').textContent.indexOf('丢弃') >= 0 && getEl('liveWarn').textContent.indexOf('超过预算') >= 0,
    getEl('liveWarn').textContent.slice(0, 50));

  api.pushEvent({ t: new Date().toISOString(), audioMs: 1116, id: 'aaaaaaaa', name: '880Hz 测试音', score: 0.9876, margin: 0.7, level: -7.4 });
  check('pushEvent 写事件时间线', getEl('eventList').children.length === 1, `${getEl('eventList').children.length} 条`);
  check('事件计数更新', getEl('eventCount').textContent === '1', getEl('eventCount').textContent);
  if (getEl('eventList').children.length === 1) {
    const texts = textOf(getEl('eventList').children[0]);
    const joined = texts.join(' | ');
    check('事件行含名称/分数/margin/电平', joined.indexOf('880Hz') >= 0 && joined.indexOf('0.9876') >= 0 && joined.indexOf('0.700') >= 0 && joined.indexOf('-7.4') >= 0,
      joined.slice(0, 100));
    check('事件行有「注册成条目」按钮', texts.some((t) => t.indexOf('注册成条目') >= 0));
    check('已在库里的命中额外提供「打开条目」', texts.some((t) => t.indexOf('打开条目') >= 0), joined.slice(-40));
  }
  api.pushEvent({ t: new Date().toISOString(), audioMs: 0, id: 'zzzzzzzz', name: '未知音效', score: 0.5, margin: null, level: null });
  check('margin/level 为 null（JSON 里的 ±inf）不崩', getEl('eventList').children.length === 2);
  check('未注册命中只提供「注册成条目」',
    textOf(getEl('eventList').children[0]).some((t) => t.indexOf('打开条目') >= 0) === false && getEl('eventList').children.length === 2);

  api.renderTick({ t: new Date().toISOString(), audioMs: 0, level: null, silent: true, top: [], event: null, stats });
  check('静音 tick 不崩（显示 -∞）', getEl('levelText').textContent.indexOf('静音') >= 0, getEl('levelText').textContent);

  // -------------------------------------------------------------------------
  // 4. SSE 回调
  // -------------------------------------------------------------------------
  api.live.disconnect();
  api.connectSSE();
  const es = EventSourceStub.last;
  check('connectSSE 打开 /api/live/stream', es && es.url === '/api/live/stream', es ? es.url : 'null');
  es.emit('hello', { running: true, source: 'WASAPI loopback [0] 扬声器', device: '扬声器', library: 'x.srz', index: 'index.bin', fingerprint: 'e01b9ba068cd5d54', tickMs: 50, topN: 8 });
  check('hello 后禁开始/可停止并显示来源',
    getEl('btnLiveStart').disabled === true && getEl('btnLiveStop').disabled === false && getEl('liveSource').textContent.indexOf('运行中') >= 0,
    getEl('liveSource').textContent.slice(0, 40));
  api.renderTick(tick);
  const before = getEl('eventList').children.length;
  es.emit('tick', tick);
  check('tick 消息驱动面板刷新', getEl('liveStats').textContent.indexOf('块 100') >= 0);
  es.emit('event', { t: new Date().toISOString(), audioMs: 2000, id: 'aaaaaaaa', name: '880Hz 测试音', score: 0.99, margin: 0.8, level: -6 });
  check('event 消息追加时间线', getEl('eventList').children.length === before + 1, `${getEl('eventList').children.length} 条`);
  es.emit('state', { running: false });
  check('state(running=false) 关闭连接并复位按钮', es.closed === true && getEl('btnLiveStop').disabled === true);

  // -------------------------------------------------------------------------
  // 5. 阈值滑块 -> PATCH
  // -------------------------------------------------------------------------
  const thrBefore = apiCalls.length;
  api.$('thrItem').value = 'aaaaaaaa';
  api.$('thrValue').value = '0.83';
  await api.applyThreshold();
  const patched = apiCalls.slice(thrBefore).find((c) => c.method === 'PATCH');
  check('应用阈值走 PATCH /api/items/{id}', !!patched && patched.url === '/api/items/aaaaaaaa', patched ? patched.url : '无 PATCH');
  check('阈值保存后有状态提示', getEl('thrStatus').textContent.indexOf('已保存') >= 0, getEl('thrStatus').textContent);

  return finish();
}

function finish() {
  console.log('');
  if (failures.length) {
    console.log(`UI 冒烟测试失败 ${failures.length}/${checks} 项:`);
    failures.forEach((f) => console.log('  - ' + f));
    process.exit(1);
  }
  console.log(`UI 冒烟测试全部通过（${checks} 项）。`);
  process.exit(0);
}

main().catch((e) => {
  console.log('  [FAIL] 测试抛异常: ' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
