/* SoundRadar 音效库管理端 —— 纯手写 JS，无任何框架/CDN 依赖。 */
'use strict';

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------
const $ = (id) => document.getElementById(id);

function fmtBytes(n) {
  if (!n && n !== 0) return '—';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KiB';
  return (n / 1024 / 1024).toFixed(2) + ' MiB';
}

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function parseTags(s) {
  return String(s || '')
    .split(/[,，;；\s]+/)
    .map((x) => x.trim())
    .filter(Boolean);
}

let bannerTimer = null;
function banner(msg, kind) {
  const el = $('banner');
  el.textContent = msg;
  el.className = 'banner ' + (kind || 'ok');
  if (bannerTimer) clearTimeout(bannerTimer);
  bannerTimer = setTimeout(() => el.classList.add('hidden'), 4500);
}

function setStatus(id, msg, kind) {
  const el = $(id);
  el.textContent = msg || '';
  el.className = 'status' + (kind ? ' ' + kind : '');
}

/** 统一 API 调用：始终解析 JSON 错误体 {"error":"..."}。 */
async function api(path, options) {
  const res = await fetch(path, options);
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch (e) { data = { error: text }; }
  }
  if (!res.ok) {
    const msg = (data && data.error) || `HTTP ${res.status}`;
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return data;
}

// ---------------------------------------------------------------------------
// 状态
// ---------------------------------------------------------------------------
const state = {
  lib: null,
  search: '',
  sort: 'created_desc',
  currentId: null,
  pendingAudio: null,
  pendingIcon: null,
};

// ---------------------------------------------------------------------------
// 路由： #/  #/new  #/item/<id>  #/live  #/candidates  #/settings
// ---------------------------------------------------------------------------
function route() {
  const h = location.hash.replace(/^#/, '') || '/';
  const itemM = h.match(/^\/item\/([0-9a-zA-Z]+)/);
  if (itemM) {
    show('viewDetail');
    live.disconnect();
    setTab('tabLib');
    loadDetail(itemM[1]);
  } else if (h === '/new') {
    show('viewCreate');
    live.disconnect();
    setTab('tabLib');
  } else if (h === '/live') {
    show('viewLive');
    setTab('tabLive');
    loadLive();
  } else if (h === '/candidates') {
    show('viewCandidates');
    setTab('tabCand');
    loadCandidates();
  } else if (h.startsWith('/candidates/')) {
    // 「注册成条目」落到这里：候选项目录 + 要聚焦的候选项 id。
    show('viewCandidates');
    setTab('tabCand');
    loadCandidates().then(() => focusCandidate(decodeURIComponent(h.slice('/candidates/'.length))));
  } else if (h === '/settings') {
    show('viewSettings');
    live.disconnect();
    setTab('tabSettings');
    loadSettings();
  } else {
    show('viewList');
    live.disconnect();
    setTab('tabLib');
  }
}

function setTab(id) {
  ['tabLib', 'tabLive', 'tabCand', 'tabSettings'].forEach((t) => $(t).classList.toggle('active', t === id));
}

function show(viewId) {
  ['viewList', 'viewCreate', 'viewDetail', 'viewLive', 'viewCandidates', 'viewSettings'].forEach((v) => {
    $(v).classList.toggle('hidden', v !== viewId);
  });
  window.scrollTo(0, 0);
}

// ---------------------------------------------------------------------------
// 库信息 + 列表
// ---------------------------------------------------------------------------
async function loadLibrary() {
  const data = await api('/api/library');
  state.lib = data;

  $('libName').textContent = data.name + `　·　schema ${data.schema}　·　创建于 ${fmtTime(data.createdAt)}`;
  $('statPath').textContent = data.path;
  $('statPath').title = data.path;
  $('statItems').textContent = data.itemCount;
  $('statSize').textContent = fmtBytes(data.fileBytes);
  $('statSamples').textContent = data.sampleCount;

  const f = data.feature || {};
  $('featureInfo').textContent =
    `特征占位（P2）：${f.kind || '?'} · ${f.sampleRate || '?'} Hz · frame ${f.frameSize || '?'} · hop ${f.hopSize || '?'} · ${f.window || '?'} · mel ${f.melBands || '?'}`;

  if (data.warnings && data.warnings.length) {
    banner('库文件有 ' + data.warnings.length + ' 条一致性提示：' + data.warnings[0].message, 'err');
  }

  renderGrid();
}

function filtered() {
  const items = (state.lib && state.lib.items) || [];
  const q = state.search.trim().toLowerCase();
  let out = items;
  if (q) {
    out = items.filter((it) => {
      const hay = [it.name, (it.tags || []).join(' '), it.note || ''].join(' ').toLowerCase();
      return hay.indexOf(q) >= 0;
    });
  }
  const s = state.sort;
  out = out.slice().sort((a, b) => {
    if (s === 'name') return a.name.localeCompare(b.name, 'zh-Hans-CN');
    if (s === 'samples') return b.sampleCount - a.sampleCount;
    const ta = new Date(a.createdAt || 0).getTime();
    const tb = new Date(b.createdAt || 0).getTime();
    return s === 'created_asc' ? ta - tb : tb - ta;
  });
  return out;
}

function renderGrid() {
  const items = filtered();
  const grid = $('grid');
  grid.innerHTML = '';
  $('listCount').textContent = `${items.length} / ${(state.lib && state.lib.itemCount) || 0} 条`;
  $('empty').classList.toggle('hidden', items.length > 0);

  for (const it of items) {
    const card = document.createElement('div');
    card.className = 'card';
    card.onclick = () => { location.hash = '#/item/' + it.id; };

    const img = document.createElement('img');
    img.src = `/api/items/${it.id}/icon.png?v=${encodeURIComponent(it.updatedAt || '')}`;
    img.alt = it.name;
    img.loading = 'lazy';
    card.appendChild(img);

    const name = document.createElement('div');
    name.className = 'name';
    name.textContent = it.name;
    card.appendChild(name);

    const meta = document.createElement('div');
    meta.className = 'meta';
    const left = document.createElement('span');
    left.textContent = `样本 ${it.sampleCount}`;
    const right = document.createElement('span');
    right.textContent = `阈值 ${Number(it.threshold).toFixed(2)}`;
    meta.appendChild(left);
    meta.appendChild(right);
    card.appendChild(meta);

    if (it.tags && it.tags.length) {
      const tags = document.createElement('div');
      tags.className = 'tags';
      it.tags.slice(0, 4).forEach((t) => {
        const sp = document.createElement('span');
        sp.className = 'tag';
        sp.textContent = t;
        tags.appendChild(sp);
      });
      card.appendChild(tags);
    }
    grid.appendChild(card);
  }
}

// ---------------------------------------------------------------------------
// 新增条目
// ---------------------------------------------------------------------------
function bindDrops() {
  const audioDrop = $('audioDrop');
  const audioInput = $('audioInput');
  audioDrop.onclick = () => audioInput.click();
  audioInput.onchange = () => setPendingAudio(audioInput.files[0] || null);
  ['dragenter', 'dragover'].forEach((ev) =>
    audioDrop.addEventListener(ev, (e) => { e.preventDefault(); audioDrop.classList.add('over'); }));
  ['dragleave', 'drop'].forEach((ev) =>
    audioDrop.addEventListener(ev, (e) => { e.preventDefault(); audioDrop.classList.remove('over'); }));
  audioDrop.addEventListener('drop', (e) => {
    const f = e.dataTransfer.files && e.dataTransfer.files[0];
    if (f) setPendingAudio(f);
  });

  const iconDrop = $('iconDrop');
  const iconInput = $('iconInput');
  iconDrop.onclick = () => iconInput.click();
  iconInput.onchange = () => setPendingIcon(iconInput.files[0] || null);
  ['dragenter', 'dragover'].forEach((ev) =>
    iconDrop.addEventListener(ev, (e) => { e.preventDefault(); iconDrop.classList.add('over'); }));
  ['dragleave', 'drop'].forEach((ev) =>
    iconDrop.addEventListener(ev, (e) => { e.preventDefault(); iconDrop.classList.remove('over'); }));
  iconDrop.addEventListener('drop', (e) => {
    const f = e.dataTransfer.files && e.dataTransfer.files[0];
    if (f) setPendingIcon(f);
  });
}

function setPendingAudio(f) {
  state.pendingAudio = f;
  const el = $('audioName');
  if (!f) { el.textContent = '未选择文件'; el.classList.remove('set'); return; }
  el.textContent = `${f.name}（${fmtBytes(f.size)}）`;
  el.classList.add('set');
  if (!$('cName').value.trim()) {
    $('cName').value = f.name.replace(/\.[^.]+$/, '');
  }
}

function setPendingIcon(f) {
  state.pendingIcon = f;
  const img = $('iconPreview');
  if (!f) { img.classList.add('hidden'); $('iconHint').classList.remove('hidden'); return; }
  img.src = URL.createObjectURL(f);
  img.classList.remove('hidden');
  $('iconHint').classList.add('hidden');
}

async function submitCreate(e) {
  e.preventDefault();
  if (!state.pendingAudio) { setStatus('createStatus', '请先选择音频文件', 'err'); return; }
  const name = $('cName').value.trim();
  if (!name) { setStatus('createStatus', '名称必填', 'err'); return; }

  const fd = new FormData();
  fd.append('audio', state.pendingAudio, state.pendingAudio.name);
  fd.append('name', name);
  if (state.pendingIcon) fd.append('icon', state.pendingIcon, state.pendingIcon.name);
  fd.append('tags', $('cTags').value);
  fd.append('note', $('cNote').value);
  fd.append('threshold', $('cThreshold').value);
  fd.append('cooldownMs', $('cCooldown').value);
  fd.append('profile', $('cProfile').value);

  const btn = $('btnCreate');
  btn.disabled = true;
  setStatus('createStatus', '上传中…');
  try {
    const created = await api('/api/items', { method: 'POST', body: fd });
    banner(`已添加「${created.name}」，id=${created.id}`, 'ok');
    resetCreateForm();
    await loadLibrary();
    location.hash = '#/item/' + created.id;
  } catch (err) {
    setStatus('createStatus', '失败：' + err.message, 'err');
    banner('新增失败：' + err.message, 'err');
  } finally {
    btn.disabled = false;
  }
}

function resetCreateForm() {
  $('createForm').reset();
  $('cThreshold').value = '0.72';
  $('cCooldown').value = '400';
  $('cProfile').value = 'default';
  setPendingAudio(null);
  setPendingIcon(null);
  setStatus('createStatus', '');
}

// ---------------------------------------------------------------------------
// 条目详情
// ---------------------------------------------------------------------------
let detailSamples = [];

async function loadDetail(id) {
  state.currentId = id;
  try {
    const it = await api('/api/items/' + id);
    detailSamples = it.samples || [];
    $('dIcon').src = `/api/items/${id}/icon.png?v=${Date.now()}`;
    $('dName').textContent = it.name;
    $('dIds').textContent = `id ${it.id} · 创建 ${fmtTime(it.createdAt)} · 更新 ${fmtTime(it.updatedAt)} · 图标 ${it.icon}`;
    $('dNameInput').value = it.name;
    $('dTagsInput').value = (it.tags || []).join(',');
    $('dThreshold').value = it.threshold;
    $('dCooldown').value = it.cooldownMs;
    $('dProfile').value = it.profile || 'default';
    $('dNote').value = it.note || '';
    $('dIconInput').value = '';
    renderDetailTags(it.tags || []);
    renderSamples();
  } catch (err) {
    banner('加载条目失败：' + err.message, 'err');
    location.hash = '#/';
  }
}

function renderDetailTags(tags) {
  const box = $('dTags');
  box.innerHTML = '';
  tags.forEach((t) => {
    const sp = document.createElement('span');
    sp.className = 'tag';
    sp.textContent = t;
    box.appendChild(sp);
  });
}

function renderSamples() {
  const tb = $('sampleRows');
  tb.innerHTML = '';
  $('dSampleCount').textContent = detailSamples.length;
  detailSamples.forEach((sm, i) => {
    const tr = document.createElement('tr');

    const tdN = document.createElement('td');
    tdN.textContent = String(i + 1);
    tr.appendChild(tdN);

    const tdF = document.createElement('td');
    tdF.className = 'mono';
    tdF.textContent = sm.file;
    tr.appendChild(tdF);

    const tdL = document.createElement('td');
    tdL.textContent = (sm.lenS != null ? sm.lenS.toFixed(3) : '?') + ' s';
    tr.appendChild(tdL);

    const tdO = document.createElement('td');
    const o = sm.origin || {};
    tdO.textContent = `${o.container || '?'} · ${o.sampleRate || '?'} Hz · ${o.channels || '?'} ch` +
      (o.resampled ? ' （已重采样）' : '') + (o.downmixed ? ' （已混音）' : '');
    tr.appendChild(tdO);

    const tdA = document.createElement('td');
    tdA.textContent = fmtTime(sm.addedAt);
    tr.appendChild(tdA);

    const tdP = document.createElement('td');
    const au = document.createElement('audio');
    au.controls = true;
    au.preload = 'none';
    au.src = `/api/items/${state.currentId}/samples/${i + 1}.wav`;
    tdP.appendChild(au);
    tr.appendChild(tdP);

    const tdD = document.createElement('td');
    const del = document.createElement('button');
    del.className = 'btn danger';
    del.textContent = '删除';
    del.onclick = () => deleteSample(i);
    tdD.appendChild(del);
    tr.appendChild(tdD);

    tb.appendChild(tr);
  });
}

async function saveDetail() {
  const body = {
    name: $('dNameInput').value.trim(),
    tags: parseTags($('dTagsInput').value),
    note: $('dNote').value,
    threshold: Number($('dThreshold').value),
    cooldownMs: Number($('dCooldown').value),
    profile: $('dProfile').value.trim() || 'default',
  };
  const iconFile = $('dIconInput').files[0];
  let opts;
  if (iconFile) {
    const fd = new FormData();
    fd.append('patch', JSON.stringify(body));
    fd.append('icon', iconFile, iconFile.name);
    opts = { method: 'PATCH', body: fd };
  } else {
    opts = {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    };
  }
  setStatus('detailStatus', '保存中…');
  try {
    const it = await api('/api/items/' + state.currentId, opts);
    setStatus('detailStatus', '已保存', 'ok');
    banner('已保存修改', 'ok');
    loadDetail(it.id);
    loadLibrary();
  } catch (err) {
    setStatus('detailStatus', '失败：' + err.message, 'err');
  }
}

async function deleteItem() {
  const name = $('dName').textContent;
  if (!confirm(`确定删除条目「${name}」？该条目及其所有样本都会从库中移除。`)) return;
  try {
    await api('/api/items/' + state.currentId, { method: 'DELETE' });
    banner('条目已删除', 'ok');
    location.hash = '#/';
    await loadLibrary();
  } catch (err) {
    banner('删除失败：' + err.message, 'err');
  }
}

async function addSample() {
  const f = $('sampleInput').files[0];
  if (!f) { setStatus('sampleStatus', '请选择音频文件', 'err'); return; }
  const fd = new FormData();
  fd.append('audio', f, f.name);
  setStatus('sampleStatus', '上传中…');
  try {
    const it = await api(`/api/items/${state.currentId}/samples`, { method: 'POST', body: fd });
    $('sampleInput').value = '';
    detailSamples = it.samples || [];
    setStatus('sampleStatus', `已追加（共 ${detailSamples.length} 个）`, 'ok');
    renderSamples();
    loadLibrary();
  } catch (err) {
    setStatus('sampleStatus', '失败：' + err.message, 'err');
  }
}

async function deleteSample(i) {
  if (!confirm(`删除样本 ${i + 1}？剩余样本会重新编号。`)) return;
  try {
    await api(`/api/items/${state.currentId}/samples/${i + 1}`, { method: 'DELETE' });
    const it = await api('/api/items/' + state.currentId);
    detailSamples = it.samples || [];
    renderSamples();
    banner('样本已删除', 'ok');
    loadLibrary();
  } catch (err) {
    banner('删除样本失败：' + err.message, 'err');
  }
}

// ---------------------------------------------------------------------------
// 实时打分面板（SSE）
// ---------------------------------------------------------------------------
const GATE_DBFS = -60;   // 与 match.DefaultOptions().SilenceDBFS 一致
const FLOOR_DBFS = -60;  // 电平条左端

const live = {
  es: null,           // EventSource
  running: false,
  source: null,       // /api/live 的最新状态
  peak: -Infinity,
  peakAt: 0,
  events: [],         // 最近命中（最新的在前）
  lastTickAt: 0,
  tickCount: 0,
};

/** dBFS -> 电平条百分比（-60..0 映射到 0..100）。 */
function dbfsPct(db) {
  if (!isFinite(db)) return 0;
  const p = ((db - FLOOR_DBFS) / (0 - FLOOR_DBFS)) * 100;
  return Math.max(0, Math.min(100, p));
}

/** 分数 -> 颜色：0.5 蓝、0.75 青、0.9 绿。 */
function scoreColor(s) {
  const t = Math.max(0, Math.min(1, (s - 0.5) / 0.45));
  const hue = 210 - 150 * t;
  return `hsl(${hue.toFixed(0)}, 70%, ${(38 + 18 * t).toFixed(0)}%)`;
}

function bindLive() {
  $('btnLiveStart').onclick = startLive;
  $('btnLiveStop').onclick = stopLive;
  $('btnThrApply').onclick = applyThreshold;
  $('thrItem').onchange = () => {
    const it = findItem($('thrItem').value);
    if (it) { $('thrValue').value = it.threshold; $('thrText').textContent = Number(it.threshold).toFixed(2); }
  };
  $('thrValue').oninput = () => { $('thrText').textContent = Number($('thrValue').value).toFixed(2); };
}

function findItem(id) {
  return ((state.lib && state.lib.items) || []).find((it) => it.id === id) || null;
}

async function loadLive() {
  // 直接进实时页签（或初始加载库失败后重试）时，先确保库已加载：
  // 阈值下拉与「打开条目」都依赖 state.lib。
  if (!state.lib) {
    try { await loadLibrary(); } catch (e) { /* 横幅已经提示过了 */ }
  }
  renderThresholdItems();
  try {
    const dev = await api('/api/live/devices');
    renderDevices(dev);
    if (dev.error) {
      setText('liveError', '设备枚举失败（可改用文件回放）：' + dev.error);
    }
  } catch (err) {
    setText('liveError', '设备枚举失败：' + err.message);
  }
  try {
    const st = await api('/api/live');
    applyLiveState(st);
  } catch (err) {
    setText('liveError', '读取状态失败：' + err.message);
  }
}

function setText(id, s) { const el = $(id); if (el) el.textContent = s || ''; }

function renderDevices(d) {
  const sel = $('liveDevice');
  const keep = sel.value;
  sel.innerHTML = '';
  const def = document.createElement('option');
  def.value = '';
  def.textContent = '默认端点（系统默认播放设备）';
  sel.appendChild(def);
  (d.devices || []).forEach((dev) => {
    const o = document.createElement('option');
    o.value = dev.name;
    o.textContent = `[${dev.index}] ${dev.name}` + (dev.default ? '（当前默认）' : '') + (dev.defaultComms ? '（通信默认）' : '');
    sel.appendChild(o);
  });
  if (keep) sel.value = keep;
}

function renderThresholdItems() {
  const sel = $('thrItem');
  const keep = sel.value;
  sel.innerHTML = '';
  const items = (state.lib && state.lib.items) || [];
  if (!items.length) {
    const o = document.createElement('option');
    o.value = '';
    o.textContent = '（库里还没有条目）';
    sel.appendChild(o);
    return;
  }
  items.forEach((it) => {
    const o = document.createElement('option');
    o.value = it.id;
    o.textContent = `${it.name}（阈值 ${Number(it.threshold).toFixed(2)}）`;
    sel.appendChild(o);
  });
  if (keep && findItem(keep)) sel.value = keep;
  const cur = findItem(sel.value) || items[0];
  if (cur) {
    sel.value = cur.id;
    $('thrValue').value = cur.threshold;
    $('thrText').textContent = Number(cur.threshold).toFixed(2);
  }
}

async function startLive() {
  setText('liveError', '');
  const body = {
    device: $('liveDevice').value || '',
    wav: $('liveWav').value.trim(),
    tickMs: Number($('liveTickMs').value) || 50,
    topN: Number($('liveTopN').value) || 8,
  };
  const btn = $('btnLiveStart');
  btn.disabled = true;
  try {
    const st = await api('/api/live/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    live.events = [];
    renderEvents();
    applyLiveState(st);
    connectSSE();
    banner('实时识别已开始', 'ok');
  } catch (err) {
    setText('liveError', '启动失败：' + err.message);
  } finally {
    btn.disabled = false;
  }
}

async function stopLive() {
  try {
    const st = await api('/api/live/stop', { method: 'POST' });
    live.disconnect();
    applyLiveState(st);
    banner('实时识别已停止', 'ok');
  } catch (err) {
    setText('liveError', '停止失败：' + err.message);
  }
}

live.disconnect = function disconnect() {
  if (live.es) { live.es.close(); live.es = null; }
  live.running = false;
  $('btnLiveStart').disabled = false;
  $('btnLiveStop').disabled = true;
};

function connectSSE() {
  live.disconnect();
  const es = new EventSource('/api/live/stream');
  live.es = es;
  es.addEventListener('hello', (e) => {
    const d = JSON.parse(e.data);
    live.running = !!d.running;
    $('btnLiveStart').disabled = true;
    $('btnLiveStop').disabled = false;
    setText('liveSource', `运行中：${d.source || d.device || '（未知音源）'}　库 ${d.library || '—'}　指纹 ${String(d.fingerprint || '').slice(0, 12)}…`);
  });
  es.addEventListener('tick', (e) => {
    const d = JSON.parse(e.data);
    live.tickCount++;
    live.lastTickAt = Date.now();
    if (d.event) pushEvent(d.event);
    renderTick(d);
  });
  es.addEventListener('event', (e) => pushEvent(JSON.parse(e.data)));
  es.addEventListener('recall', (e) => {
    // P4：有人（热键 / API / 另一个标签页）保存了候选项，徽标立刻 +1。
    try {
      const d = JSON.parse(e.data);
      const n = ((candidates.data && candidates.data.count) || 0) + 1;
      renderCandBadge(n);
      const peak = (d.peakDbfs === null || d.peakDbfs === undefined) ? '-∞' : Number(d.peakDbfs).toFixed(2);
      banner(`已保存候选项 ${d.id}（${Number(d.seconds).toFixed(2)} 秒，峰值 ${peak} dBFS）`, 'ok');
      if (location.hash === '#/candidates') loadCandidates();
      else if (candidates.data) candidates.data.count = n;
    } catch (err) { /* 事件损坏不影响面板 */ }
  });
  es.addEventListener('state', (e) => {
    const d = JSON.parse(e.data);
    if (d.running === false) {
      live.disconnect();
      setText('liveSource', d.error ? ('会话结束：' + d.error) : '会话已结束。');
      api('/api/live').then(applyLiveState).catch(() => {});
    }
  });
  es.onerror = () => {
    // EventSource 会自动重连；服务停止时后端会发 state 事件。
    if (live.running) setText('liveError', 'SSE 连接中断，正在重连…');
  };
}

function applyLiveState(st) {
  live.source = st;
  live.running = !!st.running;
  $('btnLiveStart').disabled = live.running;
  $('btnLiveStop').disabled = !live.running;
  if (st.source) {
    setText('liveSource', `${st.running ? '运行中' : '已停止'}：${st.source}　库 ${st.library || '—'}`);
  }
  if (st.error) setText('liveError', st.error);
  if (st.events && st.events.length && !live.events.length) {
    live.events = st.events.slice();
    renderEvents();
  }
  if (st.tick) renderTick(st.tick);
  else if (st.stats) renderStats(st.stats, null);
  // 进入页面时如果会话已经在跑（例如命令行或另一个标签页启动的），直接接上 SSE。
  if (live.running && !live.es) connectSSE();
}

function renderTick(t) {
  const db = t.level;
  $('levelText').textContent = (db === null || db === undefined) ? '静音（-∞）' : Number(db).toFixed(1) + ' dBFS';
  $('levelFill').style.width = dbfsPct(db) + '%';

  const now = Date.now();
  if (typeof db === 'number' && db > live.peak) { live.peak = db; live.peakAt = now; }
  // 峰值保持 1.5 s，然后按 20 dB/s 回落。
  const decay = Math.max(0, (now - live.peakAt - 1500) / 1000) * 20;
  const peakShown = live.peak - decay;
  $('levelPeak').style.left = dbfsPct(peakShown) + '%';
  $('levelGate').style.left = dbfsPct(GATE_DBFS) + '%';

  renderBars(t.top || [], t.silent);
  renderStats(t.stats, t);
}

function renderStats(st, tick) {
  if (!st) return;
  const parts = [
    `块 ${st.blocks}`,
    `音频 ${Number(st.audioSeconds || 0).toFixed(1)} s`,
    `窗口 ${st.windows}`,
    `tick ${st.ticks}`,
    `命中 ${st.events}`,
    `丢帧 ${st.dropped}`,
    `单块 平均 ${Number(st.avgBlockMs || 0).toFixed(2)} ms / 峰值 ${Number(st.maxBlockMs || 0).toFixed(2)} ms`,
    `每 20 ms 音频 ${Number(st.msPer20msAudio || 0).toFixed(2)} ms（预算 ${st.budgetMs} ms）`,
    `堆 ${fmtBytes(st.heapInuseBytes)}`,
  ];
  if (tick && tick.silent) parts.push('本 tick 静音（沿用上一次排名）');
  $('liveStats').textContent = parts.join('　·　');

  const warns = [];
  if (st.dropped > 0) warns.push(`⚠ 有 ${st.dropped} 个音频块因处理不过来被丢弃（面板仍可用，但可能漏掉命中）`);
  if (st.msPer20msAudio > st.budgetMs) warns.push(`⚠ 每 20 ms 音频耗时 ${Number(st.msPer20msAudio).toFixed(2)} ms，超过预算 ${st.budgetMs} ms`);
  if (st.silentWindows > 0 && st.windows > 0 && st.silentWindows === st.windows) warns.push('⚠ 至今所有窗口都低于静音门限：检查采集端点是不是选错了');
  $('liveWarn').textContent = warns.join('　');
}

function renderBars(top, silent) {
  const box = $('topBars');
  if (!top.length) {
    if (!box.dataset.empty) {
      box.innerHTML = '<div class="empty">还没有分数（静音或索引为空）。</div>';
      box.dataset.empty = '1';
    }
    return;
  }
  delete box.dataset.empty;
  if (box.children.length !== top.length || box.dataset.mode !== 'bars') {
    box.innerHTML = '';
    top.forEach(() => {
      const row = document.createElement('div');
      row.className = 'barRow';
      row.innerHTML = '<div class="barName"></div><div class="barTrack"><div class="barFill"></div></div><div class="barScore"></div>';
      box.appendChild(row);
    });
    box.dataset.mode = 'bars';
  }
  top.forEach((h, i) => {
    const row = box.children[i];
    row.classList.toggle('lead', i === 0);
    const name = row.querySelector('.barName');
    const label = `#${i + 1} ${h.name}`;
    if (name.textContent !== label) name.textContent = label;
    name.title = h.id;
    const fill = row.querySelector('.barFill');
    fill.style.width = Math.max(0, Math.min(1, h.score)) * 100 + '%';
    fill.style.background = scoreColor(h.score);
    fill.style.opacity = silent ? 0.45 : 1;
    const sc = row.querySelector('.barScore');
    const t = Number(h.score).toFixed(4);
    if (sc.textContent !== t) sc.textContent = t;
  });
}

function pushEvent(ev) {
  live.events.unshift(ev);
  if (live.events.length > 50) live.events.length = 50;
  renderEvents();
  banner(`命中「${ev.name}」相似度 ${Number(ev.score).toFixed(3)}`, 'ok');
}

function renderEvents() {
  const list = $('eventList');
  $('eventCount').textContent = live.events.length;
  list.innerHTML = '';
  live.events.forEach((ev) => {
    const li = document.createElement('li');
    const icon = document.createElement('span');
    icon.className = 'evIcon';
    icon.textContent = '🔔';
    li.appendChild(icon);

    const name = document.createElement('span');
    name.className = 'evName';
    name.textContent = ev.name;
    li.appendChild(name);

    const meta = document.createElement('span');
    meta.className = 'evMeta';
    const when = ev.t ? new Date(ev.t) : null;
    const hhmmss = when && !isNaN(when.getTime())
      ? when.toLocaleTimeString('zh-CN', { hour12: false }) + '.' + String(when.getMilliseconds()).padStart(3, '0')
      : '—';
    const margin = (ev.margin === null || ev.margin === undefined) ? '—' : Number(ev.margin).toFixed(3);
    const level = (ev.level === null || ev.level === undefined) ? '-∞' : Number(ev.level).toFixed(1);
    meta.textContent = `${hhmmss}　分数 ${Number(ev.score).toFixed(4)}　margin ${margin}　电平 ${level} dBFS　音频位置 ${Number(ev.audioMs || 0).toFixed(0)} ms`;
    li.appendChild(meta);

    const actions = document.createElement('span');
    actions.className = 'evActions';
    if (findItem(ev.id)) {
      const open = document.createElement('button');
      open.className = 'btn';
      open.textContent = '打开条目';
      open.onclick = () => { location.hash = '#/item/' + ev.id; };
      actions.appendChild(open);
    }
    const reg = document.createElement('button');
    reg.className = 'btn';
    reg.textContent = '注册成条目';
    reg.title = '如果最近的候选项还在收件箱里，直接跳到它的录入表单（否则退回手动新增）';
    reg.onclick = () => prefillNewItem(ev);
    actions.appendChild(reg);
    li.appendChild(actions);

    list.appendChild(li);
  });
}

/** 「注册成条目」：如果收件箱里有候选项就跳到它的录入表单，否则退回手动新增。 */
async function prefillNewItem(ev) {
  // P4：先看有没有"刚才那几秒"。命中最近的候选项时，用户只要填个名字就能入库，
  // 这是 P4 想要的效果；没有再退回旧行为（预填名称 + 手动上传音频）。
  try {
    const d = await api('/api/candidates');
    renderCandBadge(d.count);
    const items = (d && d.items) || [];
    if (items.length) {
      const top = items[0];
      renderCandBadge(d.count);
      candidates.data = d;
      banner('收件箱里有候选项，已跳到它的录入表单（名称已按命中预填）', 'ok');
      location.hash = '#/candidates/' + encodeURIComponent(top.id);
      return;
    }
  } catch (err) { /* 读不到就退回旧行为 */ }

  resetCreateForm();
  setPendingAudio(null);
  $('cName').value = ev.name;
  $('cTags').value = '实时命中';
  const when = ev.t ? new Date(ev.t) : new Date();
  $('cNote').value = `实时打分面板命中：分数 ${Number(ev.score).toFixed(4)}，`
    + `margin ${ev.margin === null || ev.margin === undefined ? '—' : Number(ev.margin).toFixed(3)}，`
    + `电平 ${ev.level === null ? '-∞' : Number(ev.level).toFixed(1)} dBFS，时间 ${when.toLocaleString('zh-CN')}。\n`
    + '音频请在这里上传（收件箱里还没有候选项，按一下回溯热键就能自动存下来）。';
  location.hash = '#/new';
  banner('收件箱里没有候选项，已预填名称，请选择这次命中对应的音频文件', 'ok');
}

/** 聚焦到某一个候选项卡片（跳转过来时高亮并滚动过去）。 */
function focusCandidate(id) {
  const card = document.querySelector(`.candCard[data-id="${id}"]`);
  if (!card) {
    banner('候选项 ' + id + ' 已经不在收件箱里了（可能已被录入或丢弃）', 'err');
    return;
  }
  card.classList.add('focus');
  card.scrollIntoView({ behavior: 'smooth', block: 'center' });
  const nameEl = $('candName_' + id);
  if (nameEl) nameEl.focus();
}

async function applyThreshold() {
  const id = $('thrItem').value;
  if (!id) { setStatus('thrStatus', '请先选择一个条目', 'err'); return; }
  const v = Number($('thrValue').value);
  setStatus('thrStatus', '保存中…');
  try {
    const it = await api('/api/items/' + id, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ threshold: v }),
    });
    setStatus('thrStatus', `已保存（${Number(it.threshold).toFixed(2)}），实时链路立即生效`, 'ok');
    await loadLibrary();
    renderThresholdItems();
  } catch (err) {
    setStatus('thrStatus', '失败：' + err.message, 'err');
  }
}

// ---------------------------------------------------------------------------
// 设置面板（采集端点 / 悬浮窗位置与大小 / 显示行为 / 热键）
// ---------------------------------------------------------------------------
// 数据来源：
//   GET   /api/config          配置文件位置 + 完整文档
//   PATCH /api/config          部分更新，立即生效，返回 effects[]
//   GET   /api/overlay         悬浮窗实时状态（没有悬浮窗时 available=false）
//   POST  /api/overlay/preview 立刻弹一个条目做预览
//   POST  /api/overlay/visible 显示 / 隐藏
//   GET   /api/live/devices    采集端点列表
const ANCHORS = [
  { id: 'anchorTopLeft', value: 'top-left', label: '左上' },
  { id: 'anchorTopCenter', value: 'top-center', label: '上中' },
  { id: 'anchorTopRight', value: 'top-right', label: '右上' },
  { id: 'anchorMiddleLeft', value: 'middle-left', label: '左中' },
  { id: 'anchorCenter', value: 'center', label: '居中' },
  { id: 'anchorMiddleRight', value: 'middle-right', label: '右中' },
  { id: 'anchorBottomLeft', value: 'bottom-left', label: '左下' },
  { id: 'anchorBottomCenter', value: 'bottom-center', label: '下中' },
  { id: 'anchorBottomRight', value: 'bottom-right', label: '右下' },
];

const settings = {
  config: null,   // GET /api/config 的最新响应
  overlay: null,  // GET /api/overlay 的最新响应
  anchor: '',     // 表单里选中的锚点（data-anchor 的值）
};

function bindSettings() {
  ANCHORS.forEach((a) => {
    const btn = $(a.id);
    if (btn) btn.onclick = () => setAnchor(a.value);
  });
  $('btnSaveDevice').onclick = saveDevice;
  $('btnApplyOverlay').onclick = applyOverlaySettings;
  $('btnPreview').onclick = previewOverlay;
  $('btnToggleOverlay').onclick = toggleOverlayVisible;
  $('btnSaveHotkey').onclick = saveHotkey;
  $('btnSaveRecall').onclick = saveRecall;
  $('setSize').oninput = () => { $('setSizeText').textContent = $('setSize').value + ' px'; };
  $('setOpacity').oninput = () => { $('setOpacityText').textContent = Number($('setOpacity').value).toFixed(2); };
}

function setAnchor(anchor) {
  settings.anchor = anchor || '';
  ANCHORS.forEach((a) => {
    const btn = $(a.id);
    if (btn) btn.classList.toggle('active', a.value === settings.anchor);
  });
}

/** 数字 -> 显示用的字符串（null / undefined / NaN 都显示 —）。 */
function numText(v) {
  if (v === null || v === undefined) return '—';
  const n = Number(v);
  return isNaN(n) ? '—' : String(n);
}

/** 32 位无符号 -> 0x 十六进制。 */
function hex32(v) {
  if (v === null || v === undefined) return '—';
  const n = Number(v);
  if (isNaN(n)) return '—';
  return '0x' + (n >>> 0).toString(16).toUpperCase();
}

async function loadSettings() {
  setStatus('setStatus', '');
  setStatus('setDeviceStatus', '');
  setStatus('setHotkeyStatus', '');
  setText('setOverlayNote', '');

  // 预览下拉需要库里的条目；从别的标签页直接进来时 state.lib 可能还是空的。
  if (!state.lib) {
    try { await loadLibrary(); } catch (e) { /* 横幅已经提示过了 */ }
  }
  renderPreviewItems();

  let cfg = null;
  try {
    cfg = await api('/api/config');
  } catch (err) {
    setStatus('setStatus', '读取配置失败：' + err.message, 'err');
  }
  settings.config = cfg;
  if (cfg) applyConfigToForm(cfg);

  try {
    const dev = await api('/api/live/devices');
    renderSettingsDevices(dev, cfg);
  } catch (err) {
    setStatus('setDeviceStatus', '设备枚举失败：' + err.message, 'err');
  }

  try {
    const ov = await api('/api/overlay');
    settings.overlay = ov;
    applyOverlayState(ov, cfg);
  } catch (err) {
    // 悬浮窗状态读不到也不能让整页崩掉：下面的控件保持可用，只提示一下。
    setText('setOverlayState', '读取悬浮窗状态失败：' + err.message);
  }

  await loadRecallStatus();
}

/** 把 GET /api/config 的文档填进表单。 */
function applyConfigToForm(cfg) {
  const c = (cfg && cfg.config) || {};
  const o = c.overlay || {};
  const h = c.hotkeys || {};

  $('setX').value = numText(o.x) === '—' ? 0 : o.x;
  $('setY').value = numText(o.y) === '—' ? 0 : o.y;
  $('setMargin').value = numText(o.margin) === '—' ? 0 : o.margin;

  const sizeEl = $('setSize');
  const size = numText(o.size) === '—' ? 96 : Number(o.size);
  // 允许配置里比滑块的默认上限更大（Validate 允许到 1024），否则会被静默截断。
  if (size > Number(sizeEl.max || 320)) sizeEl.max = String(size);
  sizeEl.value = size;
  $('setSizeText').textContent = size + ' px';

  const opacity = numText(o.opacity) === '—' ? 1 : Number(o.opacity);
  $('setOpacity').value = opacity;
  $('setOpacityText').textContent = opacity.toFixed(2);

  $('setDuration').value = numText(o.durationMs) === '—' ? 1150 : o.durationMs;
  $('setFadeIn').value = numText(o.fadeInMs) === '—' ? 0 : o.fadeInMs;
  $('setFadeOut').value = numText(o.fadeOutMs) === '—' ? 0 : o.fadeOutMs;
  $('setMaxSim').value = numText(o.maxSimultaneous) === '—' ? 1 : o.maxSimultaneous;
  $('setShowName').checked = !!o.showName;
  $('setShowScore').checked = !!o.showScore;
  $('setHotkeyInput').value = h.toggleOverlay || 'none';
  setAnchor(o.anchor || '');

  // P4：回溯保存
  const r = c.recall || {};
  $('setRecallHotkey').value = h.recallLabel || 'none';
  $('setRecallSeconds').value = numText(r.seconds) === '—' ? 3 : r.seconds;
  $('setRecallMax').value = numText(r.maxFiles) === '—' ? 200 : r.maxFiles;
  $('setRecallDir').value = r.dir || 'data/candidates';
  $('setRecallEnabled').checked = r.enabled !== false;

  $('setConfigPath').textContent = `配置文件：${cfg.path}　·　来源 ${cfg.source}` +
    (cfg.writable ? '' : '　·　不可写');
}

/** 读取 /api/recall 的实时状态，写进回溯保存面板的说明行。 */
async function loadRecallStatus() {
  try {
    const st = await api('/api/recall');
    const parts = [
      st.available ? '回溯保存：可用' : '回溯保存：不可用',
      '热键 ' + (st.hotkey || '—') + (st.hotkeyRegistered ? '（RegisterHotKey 成功）' : '（未注册）'),
      '环形缓冲 ' + numText(st.ringSeconds !== undefined ? st.ringSeconds : st.seconds) + ' 秒（已覆盖 ' + numText(st.coveredSeconds) + ' 秒）',
      '收件箱 ' + numText(st.count) + ' / ' + numText(st.maxFiles),
      '目录 ' + (st.dir || '—'),
    ];
    if (st.status) parts.push(st.status);
    setText('setRecallState', parts.join('　·　'));
    renderCandBadge(st.count);
  } catch (err) {
    setText('setRecallState', '读取回溯保存状态失败：' + err.message);
  }
}

function renderSettingsDevices(dev, cfg) {
  const sel = $('setDevice');
  if (!sel) return;
  const keep = sel.value;
  sel.innerHTML = '';
  const def = document.createElement('option');
  def.value = '';
  def.textContent = '默认端点（系统默认播放设备）';
  sel.appendChild(def);
  ((dev && dev.devices) || []).forEach((d) => {
    const o = document.createElement('option');
    o.value = d.name;
    o.textContent = `[${d.index}] ${d.name}` + (d.default ? '（当前默认）' : '') + (d.defaultComms ? '（通信默认）' : '');
    sel.appendChild(o);
  });
  const cap = (cfg && cfg.config && cfg.config.capture) || {};
  sel.value = cap.device || keep || '';
  if (dev && dev.error) setStatus('setDeviceStatus', '设备枚举失败：' + dev.error, 'err');
}

function renderPreviewItems() {
  const sel = $('setPreviewItem');
  if (!sel) return;
  const keep = sel.value;
  sel.innerHTML = '';
  const items = (state.lib && state.lib.items) || [];
  const none = document.createElement('option');
  none.value = '';
  none.textContent = items.length ? '（默认：库里第一个条目）' : '（库里还没有条目）';
  sel.appendChild(none);
  items.forEach((it) => {
    const o = document.createElement('option');
    o.value = it.id;
    o.textContent = it.name;
    sel.appendChild(o);
  });
  if (keep && findItem(keep)) sel.value = keep;
}

function renderMonitorOptions(mons, current) {
  const sel = $('setMonitor');
  if (!sel) return;
  const keep = sel.value;
  sel.innerHTML = '';
  const list = mons || [];
  if (!list.length) {
    const o = document.createElement('option');
    o.value = '';
    o.textContent = '（没有显示器信息）';
    sel.appendChild(o);
    return;
  }
  list.forEach((m) => {
    const o = document.createElement('option');
    o.value = String(m.index);
    o.textContent = m.label || ('#' + m.index + ' ' + (m.device || ''));
    sel.appendChild(o);
  });
  if (current === null || current === undefined) sel.value = keep || '0';
  else sel.value = String(current);
}

/** 把 GET /api/overlay 的实时状态写进「显示 / 隐藏 · 热键 · 状态」面板。 */
function applyOverlayState(ov, cfg) {
  const st = (ov && ov.state) || {};
  const c = (cfg && cfg.config && cfg.config.overlay) || (ov && ov.config) || {};
  const available = !!(ov && ov.available);
  const visible = !!(ov && ov.visible);

  // 没有悬浮窗进程时，只读渲染，并且禁用只能对悬浮窗生效的按钮。
  $('btnApplyOverlay').disabled = !available;
  $('btnPreview').disabled = !available;
  $('btnToggleOverlay').disabled = !available;
  $('btnToggleOverlay').textContent = visible ? '隐藏悬浮窗' : '显示悬浮窗';

  setText('setOverlayNote', available ? '' : ((ov && ov.note) || '本进程没有创建悬浮窗，配置仍可编辑保存。'));

  let hot = available ? '（设置）' : '（本进程没有悬浮窗）';
  if (available) {
    hot = st.hotkeyRegistered ? '成功' : '未注册';
    if (!st.hotkeyRegistered && st.hotkeyError) hot += '：' + st.hotkeyError;
  }
  setText('setHotkey', (ov && ov.hotkey ? ov.hotkey : 'none') + '　·　' + hot);

  renderMonitorOptions(st.monitors, c.monitor);

  if (!available) {
    setText('setOverlayState', '悬浮窗：不可用（配置项仍可修改并保存）');
    return;
  }
  const parts = [
    '悬浮窗：可用',
    '可见 ' + (visible ? '是' : '否'),
    '矩形 ' + numText(st.x) + ',' + numText(st.y) + ' ' + numText(st.width) + '×' + numText(st.height),
    'HWND ' + (st.hwndHex || '—'),
    '类名 ' + (st.className || '—'),
    '帧 ' + numText(st.frames),
    '待显示 ' + numText(st.pending) + ' / 丢弃 ' + numText(st.dropped),
    '字体 ' + (st.font || '—') + (st.fontAsciiOnly ? '（仅 ASCII）' : '（支持中文）'),
    'style ' + hex32(st.style) + ' / exStyle ' + hex32(st.exStyle),
    '线程 ' + numText(st.threadId),
  ];
  setText('setOverlayState', parts.join('　·　'));
}

/** PATCH /api/config，成功后刷新配置与悬浮窗状态。 */
async function patchConfig(body, statusId) {
  setStatus(statusId, '保存中…');
  try {
    const res = await api('/api/config', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const effects = (res && res.effects) || [];
    setStatus(statusId, effects.length ? effects.join('；') : '已保存', 'ok');
    await refreshOverlay();
    return true;
  } catch (err) {
    // 服务端的 400 带中文原因，原样显示。
    setStatus(statusId, '失败：' + err.message, 'err');
    return false;
  }
}

/** 重新读取 GET /api/overlay 与 GET /api/config，让矩形 / 帧数等显示是最新的。 */
async function refreshOverlay() {
  let ov = null;
  try { ov = await api('/api/overlay'); } catch (err) { /* 状态刷不出来不影响保存结果 */ }
  if (ov) settings.overlay = ov;
  else ov = settings.overlay;

  let cfg = null;
  try { cfg = await api('/api/config'); } catch (err) { /* 同上 */ }
  if (cfg) {
    settings.config = cfg;
    applyConfigToForm(cfg);
  }
  if (ov) applyOverlayState(ov, settings.config);
}

function saveDevice() {
  const el = $('setDevice');
  return patchConfig({ capture: { device: el ? el.value : '' } }, 'setDeviceStatus');
}

function applyOverlaySettings() {
  const body = {
    overlay: {
      x: Number($('setX').value) || 0,
      y: Number($('setY').value) || 0,
      anchor: settings.anchor || 'bottom-right',
      monitor: Number($('setMonitor').value) || 0,
      margin: Number($('setMargin').value) || 0,
      size: Number($('setSize').value) || 96,
      opacity: Number($('setOpacity').value) || 0.85,
      durationMs: Number($('setDuration').value) || 0,
      fadeInMs: Number($('setFadeIn').value) || 0,
      fadeOutMs: Number($('setFadeOut').value) || 0,
      maxSimultaneous: Number($('setMaxSim').value) || 1,
      showName: !!$('setShowName').checked,
      showScore: !!$('setShowScore').checked,
    },
  };
  return patchConfig(body, 'setStatus');
}

async function previewOverlay() {
  const sel = $('setPreviewItem');
  const id = sel ? sel.value : '';
  setStatus('setStatus', '预览中…');
  try {
    const res = await api('/api/overlay/preview', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(id ? { id: id } : {}),
    });
    const name = (res && res.name) || '（未知条目）';
    const score = res && res.score !== undefined ? Number(res.score).toFixed(2) : '—';
    setStatus('setStatus', `已预览「${name}」（分数 ${score}）`, 'ok');
    await refreshOverlay();
  } catch (err) {
    setStatus('setStatus', '预览失败：' + err.message, 'err');
  }
}

async function toggleOverlayVisible() {
  const cur = !!(settings.overlay && settings.overlay.visible);
  setStatus('setStatus', '切换中…');
  try {
    const res = await api('/api/overlay/visible', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ visible: !cur }),
    });
    setStatus('setStatus', (res && res.visible) ? '悬浮窗已显示' : '悬浮窗已隐藏', 'ok');
    await refreshOverlay();
  } catch (err) {
    setStatus('setStatus', '切换失败：' + err.message, 'err');
  }
}

function saveHotkey() {
  const v = $('setHotkeyInput').value.trim() || 'none';
  return patchConfig({ hotkeys: { toggleOverlay: v } }, 'setHotkeyStatus');
}

/** PATCH /api/config 的 P4 部分：热键 + 回溯保存参数一起提交。 */
async function saveRecall() {
  const body = {
    hotkeys: { recallLabel: $('setRecallHotkey').value.trim() || 'none' },
    recall: {
      enabled: !!$('setRecallEnabled').checked,
      seconds: Number($('setRecallSeconds').value) || 3,
      dir: $('setRecallDir').value.trim() || 'data/candidates',
      maxFiles: Number($('setRecallMax').value) || 200,
    },
  };
  const ok = await patchConfig(body, 'setRecallStatus');
  if (ok) {
    await loadRecallStatus();
    banner('回溯保存设置已保存（热键改动需要重启 overlay/serve/live 才生效）', 'ok');
  }
}

// ---------------------------------------------------------------------------
// 候选项收件箱（P4：按一下热键，"刚才那 3 秒"就进来了）
// ---------------------------------------------------------------------------
// 数据来源：
//   GET    /api/candidates              收件箱 + 热键/环形缓冲状态
//   POST   /api/recall/trigger          立即保存最近 N 秒（不需要按键）
//   GET    /api/candidates/{id}.wav     试听
//   DELETE /api/candidates/{id}         丢弃
//   POST   /api/candidates/{id}/promote 录入：新建条目 / 追加到已有条目
const candidates = {
  data: null,      // GET /api/candidates 的最新响应
  busy: false,
};

function renderCandBadge(n) {
  const el = $('candBadge');
  if (!el) return;
  const count = Number(n) || 0;
  el.textContent = String(count);
  el.classList.toggle('hidden', count <= 0);
}

async function loadCandidates() {
  setStatus('recallStatus', '');
  if (!state.lib) {
    try { await loadLibrary(); } catch (e) { /* 横幅已经提示过了 */ }
  }
  let data = null;
  try {
    data = await api('/api/candidates');
  } catch (err) {
    setStatus('recallStatus', '读取候选项失败：' + err.message, 'err');
    return;
  }
  candidates.data = data;
  renderCandBadge(data.count);
  applyRecallState(data);
  renderCandidates();
}

/** 把 /api/candidates 的状态写进顶部的说明区。 */
function applyRecallState(d) {
  const available = !!(d && d.available);
  $('btnRecallNow').disabled = !available;
  if (d && d.ringSeconds) $('recallSeconds').textContent = String(d.ringSeconds);

  const parts = [];
  if (d) {
    parts.push(`热键 ${d.hotkey || '—'}：` + (d.hotkeyRegistered ? '注册成功' : '未注册'));
    parts.push(`环形缓冲 ${numText(d.ringSeconds)} 秒（当前已覆盖 ${numText(d.coveredSeconds)} 秒）`);
    parts.push(`收件箱 ${numText(d.count)} / ${numText(d.maxFiles)} 个`);
    parts.push(`目录 ${d.dir || '—'}`);
  }
  setText('recallState', parts.join('　·　'));

  if (available) {
    setText('recallNote', '按热键（不需要切出游戏）或点「立即保存」，音频立刻落盘为候选项；'
      + '在下面填个名字就入库了。');
  } else {
    setText('recallNote', (d && d.note) || '本进程没有启用回溯保存。');
  }
  setText('recallHotkey', (d && d.hotkey) || '—');
  setText('recallRing', d ? `${numText(d.ringSeconds)} 秒` : '—');
}

function renderCandidates() {
  const box = $('candList');
  const items = (candidates.data && candidates.data.items) || [];
  box.innerHTML = '';
  $('candEmpty').classList.toggle('hidden', items.length > 0);

  items.forEach((c) => {
    const card = document.createElement('div');
    card.className = 'candCard';
    card.dataset.id = c.id;

    // ---- 头部：时间 / 时长 / 峰值 / 猜测 ----
    const head = document.createElement('div');
    head.className = 'candHead';
    const title = document.createElement('div');
    title.className = 'candTitle';
    title.textContent = c.id;
    head.appendChild(title);

    const meta = document.createElement('div');
    meta.className = 'candMeta';
    const peak = (c.peakDbfs === null || c.peakDbfs === undefined)
      ? '-∞ dBFS（数字静音）'
      : Number(c.peakDbfs).toFixed(2) + ' dBFS';
    const guess = c.guessId
      ? `猜测 ${c.guessName || c.guessId}` + (c.guessScore === null || c.guessScore === undefined ? '' : ` ${Number(c.guessScore).toFixed(3)}`)
      : '无猜测';
    meta.textContent = `${fmtTime(c.createdAt)}　时长 ${Number(c.seconds).toFixed(2)} 秒　峰值 ${peak}　${guess}` + (c.source ? `　来源 ${c.source}` : '');
    head.appendChild(meta);
    card.appendChild(head);

    // ---- 试听 ----
    const audio = document.createElement('audio');
    audio.controls = true;
    audio.preload = 'none';
    audio.src = `/api/candidates/${encodeURIComponent(c.id)}.wav`;
    card.appendChild(audio);

    // ---- 录入表单 ----
    const form = document.createElement('div');
    form.className = 'candForm';

    const row1 = document.createElement('div');
    row1.className = 'grid3';
    row1.appendChild(candField('candName_' + c.id, '名称', 'text', c.guessName || ''));
    row1.appendChild(candField('candTags_' + c.id, '标签（逗号分隔）', 'text', c.guessName ? '实时命中' : ''));
    const iconField = document.createElement('div');
    iconField.className = 'field';
    const iconLabel = document.createElement('label');
    iconLabel.textContent = '图标（png / jpg，可选）';
    const iconInput = document.createElement('input');
    iconInput.type = 'file';
    iconInput.accept = '.png,.jpg,.jpeg,image/png,image/jpeg';
    iconInput.id = 'candIcon_' + c.id;
    iconField.appendChild(iconLabel);
    iconField.appendChild(iconInput);
    row1.appendChild(iconField);
    form.appendChild(row1);

    const noteField = document.createElement('div');
    noteField.className = 'field';
    const noteLabel = document.createElement('label');
    noteLabel.textContent = '备注';
    const noteInput = document.createElement('textarea');
    noteInput.rows = 2;
    noteInput.id = 'candNote_' + c.id;
    noteInput.value = buildCandidateNote(c);
    noteField.appendChild(noteLabel);
    noteField.appendChild(noteInput);
    form.appendChild(noteField);

    const row2 = document.createElement('div');
    row2.className = 'livebar';
    const modeField = document.createElement('div');
    modeField.className = 'field inline';
    const modeLabel = document.createElement('label');
    modeLabel.textContent = '入库方式';
    const modeSel = document.createElement('select');
    modeSel.id = 'candMode_' + c.id;
    const optNew = document.createElement('option');
    optNew.value = '';
    optNew.textContent = '新建条目';
    modeSel.appendChild(optNew);
    ((state.lib && state.lib.items) || []).forEach((it) => {
      const o = document.createElement('option');
      o.value = it.id;
      o.textContent = `追加到「${it.name}」（${it.sampleCount} 个样本）`;
      modeSel.appendChild(o);
    });
    modeField.appendChild(modeLabel);
    modeField.appendChild(modeSel);
    row2.appendChild(modeField);

    const actions = document.createElement('div');
    actions.className = 'liveActions';
    const promote = document.createElement('button');
    promote.className = 'btn primary';
    promote.textContent = '提交入库';
    promote.onclick = () => promoteCandidate(c.id);
    const del = document.createElement('button');
    del.className = 'btn danger';
    del.textContent = '丢弃';
    del.onclick = () => deleteCandidate(c.id);
    const status = document.createElement('span');
    status.className = 'status';
    status.id = 'candStatus_' + c.id;
    actions.appendChild(promote);
    actions.appendChild(del);
    actions.appendChild(status);
    row2.appendChild(actions);
    form.appendChild(row2);

    card.appendChild(form);
    box.appendChild(card);
  });

  // 有候选项时，让「音效库」里的条目下拉保持最新（追加目标要用）。
  renderThresholdItems();
}

function candField(id, label, type, value) {
  const wrap = document.createElement('div');
  wrap.className = 'field';
  const lab = document.createElement('label');
  lab.textContent = label;
  const input = document.createElement('input');
  input.type = type;
  input.id = id;
  input.value = value || '';
  wrap.appendChild(lab);
  wrap.appendChild(input);
  return wrap;
}

function buildCandidateNote(c) {
  const peak = (c.peakDbfs === null || c.peakDbfs === undefined)
    ? '-∞（数字静音）'
    : Number(c.peakDbfs).toFixed(2) + ' dBFS';
  let s = `回溯保存候选项 ${c.id}：${Number(c.seconds).toFixed(2)} 秒，峰值 ${peak}`;
  if (c.guessId) s += `，保存瞬间的 top-1 是「${c.guessName || c.guessId}」分数 ${Number(c.guessScore || 0).toFixed(4)}`;
  if (c.source) s += `，来源 ${c.source}`;
  return s + '。';
}

async function recallNow() {
  if (candidates.busy) return;
  candidates.busy = true;
  const btn = $('btnRecallNow');
  btn.disabled = true;
  setStatus('recallStatus', '正在保存…');
  try {
    const res = await api('/api/recall/trigger', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ source: 'api' }),
    });
    const c = res && res.candidate;
    if (c) {
      const peak = (c.peakDbfs === null || c.peakDbfs === undefined) ? '-∞' : Number(c.peakDbfs).toFixed(2);
      banner(`已保存候选项 ${c.id}（${Number(c.seconds).toFixed(2)} 秒，峰值 ${peak} dBFS）`, 'ok');
    } else {
      banner('已保存候选项', 'ok');
    }
    setStatus('recallStatus', '已保存', 'ok');
    await loadCandidates();
  } catch (err) {
    setStatus('recallStatus', '保存失败：' + err.message, 'err');
    banner('保存失败：' + err.message, 'err');
  } finally {
    candidates.busy = false;
    btn.disabled = false;
  }
}

async function deleteCandidate(id) {
  if (!confirm(`丢弃候选项 ${id}？这段音频会被删除。`)) return;
  try {
    await api('/api/candidates/' + encodeURIComponent(id), { method: 'DELETE' });
    banner('候选项已丢弃', 'ok');
    await loadCandidates();
  } catch (err) {
    setStatus('candStatus_' + id, '失败：' + err.message, 'err');
  }
}

async function promoteCandidate(id) {
  const name = (($('candName_' + id) || {}).value || '').trim();
  const tags = parseTags((($('candTags_' + id) || {}).value || ''));
  const note = (($('candNote_' + id) || {}).value || '');
  const target = (($('candMode_' + id) || {}).value || '');
  const iconFile = (($('candIcon_' + id) || {}).files || [])[0];

  if (!target && !name) {
    setStatus('candStatus_' + id, '新建条目必须填名称', 'err');
    return;
  }

  const fd = new FormData();
  fd.append('promote', JSON.stringify({ name, tags, note, targetItemId: target }));
  if (iconFile) fd.append('icon', iconFile, iconFile.name);

  setStatus('candStatus_' + id, '提交中…');
  try {
    const res = await api(`/api/candidates/${encodeURIComponent(id)}/promote`, { method: 'POST', body: fd });
    const itemId = res && (res.id || (res.item && res.item.id));
    banner((res && res.message) || `已入库（id=${itemId}）`, 'ok');
    await loadLibrary();
    await loadCandidates();
    if (res && res.action === 'created' && itemId) location.hash = '#/item/' + itemId;
  } catch (err) {
    setStatus('candStatus_' + id, '失败：' + err.message, 'err');
    banner('入库失败：' + err.message, 'err');
  }
}

function bindCandidates() {
  $('btnRecallNow').onclick = recallNow;
  $('btnRecallRefresh').onclick = () => loadCandidates().catch((e) => banner('刷新失败：' + e.message, 'err'));
}

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------
function init() {
  bindDrops();
  bindLive();
  bindSettings();
  bindCandidates();
  $('createForm').onsubmit = submitCreate;
  $('btnCreateCancel').onclick = () => { resetCreateForm(); location.hash = '#/'; };
  $('btnNew').onclick = () => { resetCreateForm(); location.hash = '#/new'; };
  $('btnReload').onclick = () => {
    loadLibrary().then(() => {
      if (location.hash === '#/live') loadLive();
      else if (location.hash === '#/candidates') loadCandidates();
      else if (location.hash === '#/settings') loadSettings();
    }).catch((e) => banner('刷新失败：' + e.message, 'err'));
  };
  $('search').oninput = (e) => { state.search = e.target.value; renderGrid(); };
  $('sort').onchange = (e) => { state.sort = e.target.value; renderGrid(); };
  $('btnBack').onclick = () => { location.hash = '#/'; };
  $('btnSave').onclick = saveDetail;
  $('btnDelete').onclick = deleteItem;
  $('btnAddSample').onclick = addSample;

  window.addEventListener('hashchange', route);
  window.addEventListener('beforeunload', () => live.disconnect());
  loadLibrary().catch((e) => banner('加载库失败：' + e.message, 'err')).then(route);
  // 待处理候选数量：标签上的徽标要在一进页面就正确。
  refreshCandidateBadge();
}

/** 只刷新徽标（进页面时用，不打开候选项页签也能看到数量）。 */
async function refreshCandidateBadge() {
  try {
    const d = await api('/api/candidates');
    candidates.data = d;
    renderCandBadge(d.count);
  } catch (err) { /* 徽标失败不影响其它功能 */ }
}

document.addEventListener('DOMContentLoaded', init);
