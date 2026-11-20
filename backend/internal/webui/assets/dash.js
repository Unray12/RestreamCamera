/* RestreamCamera — real-time dashboard + load test pages.
 * Loaded before app.js; uses its helpers (state, api, esc, fmt, toast, modal, $) at call time. */
'use strict';

const RANGES = [['1m', 60], ['5m', 300], ['15m', 900], ['1h', 3600], ['6h', 21600], ['24h', 86400]];
const CAM_METRICS = {
  out: { label: 'Băng thông ra', fmt: fmtRate, live: 'o', arr: 'out', stackable: true, emptyMax: 1000 },
  in: { label: 'Ingest', fmt: fmtRate, live: 'i', arr: 'in', stackable: true, emptyMax: 1000 },
  readers: { label: 'Viewers', fmt: fmtInt, live: 'r', arr: 'readers', stackable: true, emptyMax: 4 },
  loss: { label: 'Mất gói %', fmt: fmtPct, live: 'l', arr: 'loss', stackable: false, emptyMax: 1 },
};
const MAX_SEL = 12;

function loadPrefs() {
  const def = { range: 300, mode: 'top', topN: 5, sel: [], metric: 'out', stacked: false };
  try { return { ...def, ...JSON.parse(localStorage.getItem('rc_dash') || '{}') }; } catch { return def; }
}
function savePrefs(p) { try { localStorage.setItem('rc_dash', JSON.stringify(p)); } catch { /* ignore */ } }

const tone = (v, warn, bad) => (v >= bad ? 'bad' : v >= warn ? 'warn' : 'good');
const camSpec = (d) => (d.prefs.mode === 'top' ? 'top:' + d.prefs.topN : d.prefs.sel.join(','));

/* ------------------------------------------------------------------ lifecycle */

function dashStop() {
  const d = state.dash;
  if (d) {
    d.timers.forEach(clearInterval);
    Object.values(d.charts).forEach((c) => c.destroy());
    state.dash = null;
    if (state.es && state.sseSpec) { state.sseSpec = ''; connectSSE(''); }
  }
  const lt = state.lt;
  if (lt) {
    if (lt.timer) clearTimeout(lt.timer);
    Object.values(lt.charts || {}).forEach((c) => c.destroy());
    state.lt = null;
  }
}

async function pageDashboard() {
  const prefs = loadPrefs();
  const d = state.dash = {
    prefs, paused: false, charts: {}, timers: [], cams: [], names: [], colors: {}, colorN: 0,
    fine: prefs.range <= 900, lastLive: 0, lastSeries: 0, g: null, lt: null, camQ: '', allQ: '', allState: '', allSort: 'out', pick: '',
  };
  $('#view').innerHTML = `
    <div class="row dash-bar">
      <div class="seg" id="rangeSeg">${RANGES.map(([l, s]) => `<button data-range="${s}" class="${s === prefs.range ? 'on' : ''}">${l}</button>`).join('')}</div>
      <button id="pauseBtn" class="sm">⏸ Tạm dừng</button>
      <span id="liveBadge" class="badge">● đang kết nối…</span>
      <span class="spacer"></span>
      <span class="muted small" id="dashNote">Mẫu 1 giây · băng thông tính từ bộ đếm byte của MediaMTX</span>
    </div>
    <div class="kpis" id="kpis">
      ${kpiCard('out', 'Egress — ra client', true)}
      ${kpiCard('in', 'Ingest — từ source')}
      ${kpiCard('readers', 'Viewers')}
      ${kpiCard('cams', 'Camera ảo')}
      ${kpiCard('loss', 'Mất gói (RTSP)')}
      ${kpiCard('lat', 'Độ trễ server')}
      ${kpiCard('host', 'Máy chủ (host)')}
    </div>
    <div class="panel">
      <div class="panel-head"><h2>Băng thông tổng theo thời gian thực</h2><span class="spacer"></span>
        <span class="muted small">bấm vào chú thích để ẩn/hiện đường · rê chuột để xem giá trị</span></div>
      <div class="panel-body"><div id="chBw"></div></div>
    </div>
    <div class="chartgrid">
      <div class="panel"><div class="panel-head"><h3 class="ph">Viewers &amp; camera đang chạy</h3></div><div class="panel-body"><div id="chRd"></div></div></div>
      <div class="panel"><div class="panel-head"><h3 class="ph">Độ trễ phản hồi</h3><span class="spacer"></span><span class="muted small">RTSP OPTIONS · Control API</span></div><div class="panel-body"><div id="chLat"></div></div></div>
      <div class="panel"><div class="panel-head"><h3 class="ph">Mất gói / bị loại bỏ</h3><span class="spacer"></span><span class="muted small">% gói RTP</span></div><div class="panel-body"><div id="chLoss"></div></div></div>
      <div class="panel"><div class="panel-head"><h3 class="ph">CPU máy chủ</h3></div><div class="panel-body"><div id="chCpu"></div></div></div>
    </div>

    <div class="panel" id="camPanel">
      <div class="panel-head">
        <h2>Băng thông theo từng camera</h2><span class="spacer"></span>
        <div class="seg" id="modeSeg"><button data-mode="top">Top tải cao</button><button data-mode="manual">Tự chọn camera</button></div>
      </div>
      <div class="panel-body camsplit">
        <div class="picker" id="picker"></div>
        <div class="camchart">
          <div class="row" style="margin-bottom:8px">
            <div class="seg" id="metricSeg">${Object.entries(CAM_METRICS).map(([k, m]) => `<button data-metric="${k}">${m.label}</button>`).join('')}</div>
            <label class="chk"><input type="checkbox" id="stackChk" /> xếp chồng</label>
            <span class="spacer"></span>
            <span class="muted small" id="camNote"></span>
          </div>
          <div id="chCam"></div>
        </div>
      </div>
      <div class="tablewrap" id="watchWrap"></div>
    </div>

    <div class="panel">
      <div class="panel-head"><h2>Tất cả camera (cập nhật 2s)</h2><span class="spacer"></span>
        <input id="allQ" placeholder="Tìm slug / tên / group" style="max-width:220px" />
        <select id="allState" style="max-width:140px"><option value="">Mọi trạng thái</option>${['ready', 'idle', 'offline', 'disabled', 'error'].map((s) => `<option>${s}</option>`).join('')}</select>
        <select id="allSort" style="max-width:170px"><option value="out">Sắp xếp: băng thông</option><option value="readers">Sắp xếp: viewers</option><option value="loss">Sắp xếp: mất gói</option><option value="name">Sắp xếp: tên</option></select>
      </div>
      <div class="tablewrap" id="allWrap"></div>
    </div>

    <div class="panel" id="ltPanel"></div>`;

  const bw = (h) => ({ height: h, fmt: fmtRate, windowSec: prefs.range, emptyMax: 1000 });
  d.charts.bw = new TimeChart($('#chBw'), {
    ...bw(280),
    series: [
      { key: 'out', label: 'Egress ra client', color: '#38bdf8', width: 2 },
      { key: 'in', label: 'Ingest từ source', color: '#34d399' },
      { key: 'nicTx', label: 'NIC tx (host)', color: '#f59e0b', dash: true },
      { key: 'loop', label: 'Proxy nội bộ', color: '#a78bfa', dash: true, vis: false },
    ],
  });
  d.charts.rd = new TimeChart($('#chRd'), { height: 150, fmt: fmtInt, windowSec: prefs.range, emptyMax: 4, step: 'step',
    series: [{ key: 'readers', label: 'Viewers', color: '#a78bfa' }, { key: 'ready', label: 'Camera có media', color: '#34d399' }] });
  d.charts.lat = new TimeChart($('#chLat'), { height: 150, fmt: fmtMs, windowSec: prefs.range, emptyMax: 10,
    series: [{ key: 'rtsp', label: 'RTSP', color: '#38bdf8' }, { key: 'api', label: 'API', color: '#f59e0b' }] });
  d.charts.loss = new TimeChart($('#chLoss'), { height: 150, fmt: fmtPct, windowSec: prefs.range, emptyMax: 1,
    series: [{ key: 'loss', label: 'Mất gói', color: '#fb7185' }] });
  d.charts.cpu = new TimeChart($('#chCpu'), { height: 150, fmt: fmtPct, windowSec: prefs.range, emptyMax: 10,
    series: [{ key: 'cpu', label: 'CPU', color: '#f59e0b' }] });
  d.charts.cam = new TimeChart($('#chCam'), { height: 280, fmt: CAM_METRICS[prefs.metric].fmt, windowSec: prefs.range, legend: true, series: [] });

  bindDash(d);
  syncCamControls(d);
  paintOverviewKpis();
  await Promise.all([dashLoadCameras(), dashLoadSeries()]);
  d.timers.push(setInterval(() => { if (state.dash === d) dashLoadCameras(); }, 2000));
  d.timers.push(setInterval(() => dashTick(d), 1000));
  connectSSE(camSpec(d));
}

function kpiCard(k, title, hero) {
  return `<div class="kpi ${hero ? 'hero' : ''}" data-kpi="${k}"><h3>${title}</h3><div class="kv" data-f="v">—</div><div class="ks" data-f="s1"></div><div class="ks" data-f="s2"></div></div>`;
}
function setK(k, v, s1, s2, cls) {
  const el = document.querySelector(`[data-kpi="${k}"]`);
  if (!el) return;
  el.querySelector('[data-f=v]').textContent = v;
  el.querySelector('[data-f=s1]').innerHTML = s1 || '';
  el.querySelector('[data-f=s2]').innerHTML = s2 || '';
  el.classList.remove('good', 'warn', 'bad');
  if (cls) el.classList.add(cls);
}

function paintOverviewKpis() {
  const o = state.overview;
  if (!o) return;
  setK('cams', `${fmt(o.camerasReady)}/${fmt(o.camerasTotal)}`,
    `<span class="dot-s ready"></span>ready ${fmt(o.camerasReady)} · <span class="dot-s idle"></span>idle ${fmt(o.camerasIdle || 0)}`,
    `offline ${fmt(o.camerasOffline)} · sources ${fmt(o.sourcesOnline)}/${fmt(o.sourcesTotal)} online`,
    o.camerasOffline > 0 ? 'warn' : '');
}

function paintLiveKpis(g) {
  const per = g.readers > 0 ? g.out / g.readers : 0;
  setK('out', fmtRate(g.out), `đỉnh ${fmtRate(g.peak)}${g.peakAt && g.peak > 0 ? ' lúc ' + clock(Date.parse(g.peakAt) / 1000, true) : ''}`,
    g.readers > 0 ? `TB ${fmtRate(per)} / viewer · tổng đã gửi ${fmtBytes(g.totalOut)}` : `tổng đã gửi ${fmtBytes(g.totalOut)}`);
  setK('in', fmtRate(g.in), `proxy nội bộ ${fmtRate(g.loop)}`,
    g.in > 0 ? `fan-out ×${(g.out / g.in).toFixed(1)} · đã nhận ${fmtBytes(g.totalIn)}` : `đã nhận ${fmtBytes(g.totalIn)}`);
  const by = g.readersBy || {};
  const parts = [['rtsp', 'RTSP'], ['hls', 'HLS'], ['webrtc', 'WebRTC'], ['srt', 'SRT'], ['other', 'khác']].filter(([k]) => by[k]).map(([k, l]) => `${l} ${by[k]}`);
  setK('readers', fmt(g.readers), parts.join(' · ') || 'chưa có client', `đỉnh ${fmt(g.peakReaders)} · RTSP sessions ${fmt(g.sessions)}`);
  setK('loss', fmtPct(g.loss), `${fmt(g.sessions)} session đang đo`, 'gói mất báo qua RTCP + gói bị server loại bỏ', g.sessions ? tone(g.loss, 0.5, 2) : '');
  const rt = g.rtsp >= 0 ? g.rtsp : null;
  setK('lat', rt == null ? '—' : fmtMs(rt), `Control API ${fmtMs(g.api)}`,
    g.online ? 'MediaMTX online' : '<b style="color:var(--danger)">MediaMTX không phản hồi</b>', g.online ? tone(Math.max(rt || 0, g.api), 100, 500) : 'bad');
  if (g.hostOk) {
    setK('host', fmtRate(g.nicTx), `NIC rx ${fmtRate(g.nicRx)} · load ${g.load1}`, `CPU ${g.cpu.toFixed(0)}% · RAM ${g.mem.toFixed(0)}%`, tone(Math.max(g.cpu, g.mem), 70, 90));
  } else {
    setK('host', 'n/a', 'chỉ đo được khi chạy trên Linux', `heap API ${fmtBytes(g.heap)} · ${g.goroutines} goroutine`);
  }
}

/* ------------------------------------------------------------------ data */

async function dashLoadCameras() {
  const d = state.dash;
  if (!d) return;
  try {
    const out = await api('/monitor/cameras?sort=name');
    if (state.dash !== d) return;
    d.cams = out.items;
    d.bySlug = Object.fromEntries(out.items.map((c) => [c.slug, c]));
    paintWatch(d);
    paintAll(d);
    paintPicker(d, false);
  } catch { /* transient */ }
}

async function dashLoadSeries() {
  const d = state.dash;
  if (!d) return;
  const p = d.prefs;
  let r;
  try { r = await api(`/monitor/series?range=${p.range}&cams=${encodeURIComponent(camSpec(d))}`); } catch (e) {
    if (state.dash === d) $('#dashNote').textContent = e.message;
    return;
  }
  if (state.dash !== d) return;
  d.lastSeries = Date.now();
  d.fine = p.range <= 900;
  d.names = r.names || [];
  const g = r.global;
  const nz = (a) => a.map((v) => (v < 0 ? null : v));
  const C = d.charts;
  for (const c of Object.values(C)) c.setWindow(p.range);
  C.bw.setData(g.ts, { out: g.out, in: g.in, nicTx: g.nicTx, loop: g.loop });
  C.rd.setData(g.ts, { readers: g.readers, ready: g.ready });
  C.lat.setData(g.ts, { rtsp: nz(g.rtspMs), api: g.apiMs });
  C.loss.setData(g.ts, { loss: g.loss });
  C.cpu.setData(g.ts, { cpu: g.cpu });
  const bands = (r.marks || []).map((m) => ({ start: m.start, end: m.end, label: 'load test ' + m.mode }));
  Object.values(C).forEach((c) => c.setBands(bands));
  buildCamChart(d, r);
  paintWatch(d);
  paintPicker(d, true);
}

function colorFor(d, slug) {
  if (!d.colors[slug]) d.colors[slug] = PALETTE[d.colorN++ % PALETTE.length];
  return d.colors[slug];
}

function buildCamChart(d, r) {
  const m = CAM_METRICS[d.prefs.metric];
  const ch = d.charts.cam;
  ch.o.fmt = m.fmt;
  ch.o.stacked = !!d.prefs.stacked && m.stackable;
  ch.o.emptyMax = m.emptyMax;
  ch.setSeries(d.names.map((n) => ({ key: n, label: (d.bySlug && d.bySlug[n] ? d.bySlug[n].name + ' · ' : '') + n, color: colorFor(d, n), width: 1.7 })));
  const map = {};
  d.names.forEach((n) => { map[n] = (r.cams[n] || {})[m.arr] || []; });
  ch.setData(r.global.ts, map);
  $('#camNote').textContent = d.names.length ? `${d.names.length} camera` : d.prefs.mode === 'top' ? 'chưa camera nào có tải — mở một luồng bằng ffplay/VLC hoặc chạy load test' : 'chọn camera ở bên trái';
}

/* ------------------------------------------------------------------ SSE */

function dashOnLive(msg) {
  const g = msg.g;
  if (state.stats) { state.stats.readers = g.readers; state.stats.outKbps = g.out; state.stats.inKbps = g.in; renderTopMetrics(); }
  const d = state.dash;
  if (!d) return;
  d.lastLive = Date.now();
  d.g = g;
  d.lt = msg.lt || null;
  d.lastCamLive = msg.c;
  paintLiveKpis(g);
  paintLt(d);
  if (d.prefs.mode === 'top' && msg.top && d.fine && !d.paused && Date.now() - d.lastSeries > 6000 &&
      msg.top.slice().sort().join() !== d.names.slice().sort().join()) {
    dashLoadSeries(); // a different camera entered the top list: fetch its history
    return;
  }
  if (d.paused || !d.fine) return;
  const C = d.charts, ts = msg.ts;
  C.bw.push(ts, { out: g.out, in: g.in, nicTx: g.nicTx, loop: g.loop });
  C.rd.push(ts, { readers: g.readers, ready: g.ready });
  C.lat.push(ts, { rtsp: g.rtsp >= 0 ? g.rtsp : null, api: g.api });
  C.loss.push(ts, { loss: g.loss });
  C.cpu.push(ts, { cpu: g.cpu });
  const key = CAM_METRICS[d.prefs.metric].live;
  const vals = {};
  d.names.forEach((n) => { vals[n] = (msg.c[n] || {})[key] ?? 0; });
  C.cam.push(ts, vals);
}

function dashTick(d) {
  if (state.dash !== d) return;
  const age = (Date.now() - d.lastLive) / 1000;
  const b = $('#liveBadge');
  if (b) {
    if (d.paused) { b.className = 'badge warn'; b.textContent = '⏸ tạm dừng'; }
    else if (!d.lastLive || age > 4) { b.className = 'badge bad'; b.textContent = '⚠ mất kết nối live ' + (d.lastLive ? Math.round(age) + 's' : ''); }
    else { b.className = 'badge ok'; b.textContent = '● LIVE · 1s'; }
  }
  // 1h+ ranges come from 30 s buckets: refresh them periodically instead of pushing points
  if (!d.fine && !d.paused && Date.now() - d.lastSeries > 30000) dashLoadSeries();
}

/* ------------------------------------------------------------------ controls */

function bindDash(d) {
  const p = d.prefs;
  $('#rangeSeg').addEventListener('click', (e) => {
    const b = e.target.closest('[data-range]');
    if (!b) return;
    p.range = Number(b.dataset.range);
    savePrefs(p);
    document.querySelectorAll('#rangeSeg button').forEach((x) => x.classList.toggle('on', x === b));
    dashLoadSeries();
  });
  $('#pauseBtn').addEventListener('click', (e) => {
    d.paused = !d.paused;
    e.target.textContent = d.paused ? '▶ Tiếp tục' : '⏸ Tạm dừng';
    if (!d.paused) dashLoadSeries();
  });
  $('#modeSeg').addEventListener('click', (e) => {
    const b = e.target.closest('[data-mode]');
    if (!b || b.dataset.mode === p.mode) return;
    if (b.dataset.mode === 'manual' && !p.sel.length) p.sel = d.names.slice(0, MAX_SEL); // start from what is on screen
    p.mode = b.dataset.mode;
    savePrefs(p);
    syncCamControls(d);
    connectSSE(camSpec(d));
    dashLoadSeries();
  });
  $('#metricSeg').addEventListener('click', (e) => {
    const b = e.target.closest('[data-metric]');
    if (!b) return;
    p.metric = b.dataset.metric;
    savePrefs(p);
    syncCamControls(d);
    dashLoadSeries();
  });
  $('#stackChk').addEventListener('change', (e) => { p.stacked = e.target.checked; savePrefs(p); dashLoadSeries(); });
  $('#allQ').addEventListener('input', (e) => { d.allQ = e.target.value.toLowerCase(); paintAll(d); });
  $('#allState').addEventListener('change', (e) => { d.allState = e.target.value; paintAll(d); });
  $('#allSort').addEventListener('change', (e) => { d.allSort = e.target.value; paintAll(d); });

  $('#picker').addEventListener('input', (e) => { if (e.target.id === 'pickQ') { d.pick = e.target.value.toLowerCase(); paintPicker(d, true); } });
  $('#picker').addEventListener('click', (e) => {
    const t = e.target.closest('[data-pick]');
    if (t) { toggleCam(d, t.dataset.pick); return; }
    const a = e.target.closest('[data-pa]');
    if (!a) return;
    if (a.dataset.pa === 'clear') { p.sel = []; }
    if (a.dataset.pa === 'active') p.sel = d.cams.filter((c) => c.outKbps > 0 || c.readers > 0).sort((x, y) => y.outKbps - x.outKbps).slice(0, MAX_SEL).map((c) => c.slug);
    if (a.dataset.pa === 'topn') { p.mode = 'top'; syncCamControls(d); }
    savePrefs(p);
    connectSSE(camSpec(d));
    dashLoadSeries();
  });
  $('#camPanel').addEventListener('click', (e) => {
    const o = e.target.closest('[data-open]');
    if (o) dashCamDetail(o.dataset.open);
  });
  $('#allWrap').addEventListener('click', (e) => {
    const c = e.target.closest('[data-chart]');
    if (c) { toggleCam(d, c.dataset.chart); return; }
    const o = e.target.closest('[data-open]');
    if (o) dashCamDetail(o.dataset.open);
  });
}

function toggleCam(d, slug) {
  const p = d.prefs;
  if (p.mode === 'top') { p.mode = 'manual'; p.sel = d.names.slice(0, MAX_SEL); }
  const i = p.sel.indexOf(slug);
  if (i >= 0) p.sel.splice(i, 1);
  else if (p.sel.length >= MAX_SEL) { toast(`Tối đa ${MAX_SEL} camera trên một biểu đồ`, 'err'); return; } else p.sel.push(slug);
  savePrefs(p);
  syncCamControls(d);
  connectSSE(camSpec(d));
  dashLoadSeries();
}

function syncCamControls(d) {
  const p = d.prefs;
  document.querySelectorAll('#modeSeg button').forEach((b) => b.classList.toggle('on', b.dataset.mode === p.mode));
  document.querySelectorAll('#metricSeg button').forEach((b) => b.classList.toggle('on', b.dataset.metric === p.metric));
  const m = CAM_METRICS[p.metric];
  const chk = $('#stackChk');
  chk.checked = !!p.stacked && m.stackable;
  chk.disabled = !m.stackable;
}

/* ------------------------------------------------------------------ tables */

const dotState = (s) => `<span class="state ${esc(s)}">${esc(s)}</span>`;
const resTxt = (c) => (c.width ? `${esc(c.codec)} ${c.width}×${c.height}` : esc(c.codec || '—'));
const lossCell = (v, total) => `<span class="${v >= 2 ? 'c-bad' : v >= 0.5 ? 'c-warn' : ''}">${fmtPct(v)}</span> <span class="muted small">/ ${total.toFixed(2)}% tổng</span>`;
const protoTip = (c) => Object.entries(c.readersBy || {}).map(([k, v]) => `${k} ${v}`).join(', ');

function paintWatch(d) {
  const wrap = $('#watchWrap');
  if (!wrap) return;
  const names = d.names;
  if (!names.length) { wrap.innerHTML = ''; return; }
  wrap.innerHTML = `<table><thead><tr><th></th><th>Camera</th><th>State</th><th class="num">Viewers</th><th class="num">Ra (hiện tại)</th><th class="num">TB 1 phút</th><th class="num">Đỉnh</th><th class="num">/viewer</th><th>Mất gói</th><th>Video</th><th></th></tr></thead><tbody>
    ${names.map((n) => {
      const c = (d.bySlug || {})[n];
      if (!c) return `<tr><td><i class="cdot" style="background:${colorFor(d, n)}"></i></td><td class="slug">${esc(n)}</td><td colspan="9" class="muted">không có trong danh sách</td></tr>`;
      return `<tr><td><i class="cdot" style="background:${colorFor(d, n)}"></i></td><td><b>${esc(c.name)}</b><div class="slug">${esc(c.slug)}</div></td><td>${dotState(c.state)}</td>
        <td class="num" title="${esc(protoTip(c))}">${c.readers}</td><td class="num"><b>${fmtRate(c.outKbps)}</b></td><td class="num">${fmtRate(c.avgOutKbps)}</td><td class="num">${fmtRate(c.peakOutKbps)}</td>
        <td class="num">${c.readers ? fmtRate(c.perReaderKbps) : '—'}</td><td>${lossCell(c.lossPct, c.lossTotalPct)}</td><td>${resTxt(c)}</td>
        <td><button class="sm" data-open="${esc(c.id)}">Chi tiết</button></td></tr>`;
    }).join('')}</tbody></table>`;
}

function paintAll(d) {
  const wrap = $('#allWrap');
  if (!wrap) return;
  let rows = d.cams.filter((c) => (!d.allState || c.state === d.allState) &&
    (!d.allQ || (c.slug + ' ' + c.name + ' ' + c.group).toLowerCase().includes(d.allQ)));
  const sorts = {
    out: (a, b) => b.outKbps - a.outKbps || b.readers - a.readers || a.slug.localeCompare(b.slug),
    readers: (a, b) => b.readers - a.readers || a.slug.localeCompare(b.slug),
    loss: (a, b) => b.lossTotalPct - a.lossTotalPct || a.slug.localeCompare(b.slug),
    name: (a, b) => a.slug.localeCompare(b.slug),
  };
  rows = rows.slice().sort(sorts[d.allSort] || sorts.out);
  const shown = rows.slice(0, 120);
  const sel = new Set(d.names);
  const scroll = wrap.scrollTop;
  wrap.innerHTML = `<table><thead><tr><th title="Hiện trên biểu đồ">Biểu đồ</th><th>Camera</th><th>Group</th><th>State</th><th class="num">Viewers</th><th class="num">Ra</th><th class="num">/viewer</th><th class="num">Vào</th><th>Mất gói</th><th>Video</th><th>Chạy từ</th><th></th></tr></thead><tbody>
    ${shown.map((c) => `<tr><td><input type="checkbox" data-chart="${esc(c.slug)}" ${sel.has(c.slug) ? 'checked' : ''} /></td>
      <td><b>${esc(c.name)}</b><div class="slug">${esc(c.slug)}</div></td><td>${esc(c.group || '—')}</td><td>${dotState(c.state)}</td>
      <td class="num" title="${esc(protoTip(c))}">${c.readers}</td><td class="num">${c.outKbps > 0 ? fmtRate(c.outKbps) : '—'}</td><td class="num">${c.readers ? fmtRate(c.perReaderKbps) : '—'}</td>
      <td class="num">${c.inKbps > 0 ? fmtRate(c.inKbps) : '—'}</td><td>${c.sessions ? lossCell(c.lossPct, c.lossTotalPct) : '—'}</td><td>${resTxt(c)}</td>
      <td class="muted small">${c.since ? ago(c.since) : '—'}</td><td><button class="sm" data-open="${esc(c.id)}">Chi tiết</button></td></tr>`).join('') ||
      '<tr><td colspan="12" class="muted">Không có camera phù hợp.</td></tr>'}</tbody></table>
    ${rows.length > shown.length ? `<div class="muted small" style="padding:8px 12px">Hiển thị ${shown.length}/${rows.length} — dùng ô tìm kiếm để thu hẹp.</div>` : ''}`;
  wrap.scrollTop = scroll;
}

function paintPicker(d, force) {
  const el = $('#picker');
  if (!el) return;
  const p = d.prefs;
  if (p.mode === 'top') {
    if (!force && el.dataset.mode === 'top') return;
    el.dataset.mode = 'top';
    el.innerHTML = `<div class="hint">Tự động theo dõi <b>${p.topN}</b> camera có băng thông ra cao nhất tại thời điểm hiện tại.</div>
      <label>Số camera<select id="topN">${[3, 5, 8, 12].map((n) => `<option ${n === p.topN ? 'selected' : ''}>${n}</option>`).join('')}</select></label>
      <div class="hint">Muốn chọn camera cụ thể? Bấm “Tự chọn camera” ở trên hoặc tick ô “Biểu đồ” trong bảng bên dưới.</div>`;
    $('#topN').addEventListener('change', (e) => { p.topN = Number(e.target.value); savePrefs(p); connectSSE(camSpec(d)); dashLoadSeries(); });
    return;
  }
  const q = d.pick;
  const typing = document.activeElement && document.activeElement.id === 'pickQ';
  if (!force && typing) return; // do not rebuild while the user types
  const list = d.cams.filter((c) => !q || (c.slug + ' ' + c.name + ' ' + c.group).toLowerCase().includes(q))
    .sort((a, b) => (p.sel.includes(b.slug) - p.sel.includes(a.slug)) || b.outKbps - a.outKbps || a.slug.localeCompare(b.slug)).slice(0, 150);
  const scroll = el.querySelector('.plist')?.scrollTop || 0;
  el.dataset.mode = 'manual';
  el.innerHTML = `<input id="pickQ" placeholder="Tìm camera…" value="${esc(d.pick)}" />
    <div class="row"><button class="sm" data-pa="active">Đang có tải</button><button class="sm" data-pa="clear">Bỏ chọn</button><button class="sm ghost" data-pa="topn">Top N</button>
      <span class="muted small">${p.sel.length}/${MAX_SEL}</span></div>
    <div class="plist">${list.map((c) => `<div class="pitem ${p.sel.includes(c.slug) ? 'on' : ''}" data-pick="${esc(c.slug)}">
      <i class="cdot" style="background:${p.sel.includes(c.slug) ? colorFor(d, c.slug) : 'transparent'}"></i><span class="slug">${esc(c.slug)}</span>
      <span class="muted small pn">${esc(c.name)}</span><span class="spacer"></span><span class="small">${c.outKbps > 0 ? fmtRate(c.outKbps) : ''}</span></div>`).join('') || '<div class="muted small">Không có camera.</div>'}</div>`;
  const pl = el.querySelector('.plist');
  if (pl) pl.scrollTop = scroll;
  if (typing) { const i = $('#pickQ'); i.focus(); i.setSelectionRange(i.value.length, i.value.length); }
}

function paintLt(d) {
  const el = $('#ltPanel');
  if (!el) return;
  const l = d.lt;
  if (!l) {
    if (el.dataset.k !== 'idle') {
      el.dataset.k = 'idle';
      el.innerHTML = `<div class="panel-head"><h2>Load test</h2><span class="spacer"></span><a href="#loadtest" class="badge">▶ Chạy load test</a></div>
        <div class="panel-body muted">Chưa có load test nào đang chạy. Chế độ <b>stream</b> mở N client RTSP thật, kéo media và đo băng thông từng client, TTFB, fps, mất gói, jitter, độ trễ relay — số liệu hiện ngay trên các biểu đồ ở trên (vùng cam là thời gian chạy test).</div>`;
    }
    return;
  }
  el.dataset.k = 'run';
  const running = l.status === 'running';
  const pct = l.mode === 'stream' && l.durationSec ? Math.min(100, (l.elapsedMs / 1000 / (l.durationSec + 0)) * 100) : l.count ? (l.done / l.count) * 100 : 0;
  el.innerHTML = `<div class="panel-head"><h2>Load test ${running ? '<span class="spinner"></span>' : '(vừa xong)'}</h2><span class="muted small">${esc(l.id)} · ${esc(l.mode)}</span><span class="spacer"></span><a href="#loadtest" class="badge">Chi tiết →</a></div>
    <div class="panel-body">
      <div class="bar"><i style="width:${pct.toFixed(0)}%"></i></div>
      <div class="mini">
        <div><b>${l.active}</b><span>/${l.count} client đang nhận media</span></div>
        <div><b>${l.mbps.toFixed(1)}</b><span>Mbps tổng</span></div>
        <div><b>${l.avgKbps ? fmtRate(l.avgKbps) : '—'}</b><span>TB / client</span></div>
        <div><b class="${l.avgKbps && l.p5Kbps < l.avgKbps * 0.5 ? 'c-warn' : ''}">${l.p5Kbps ? fmtRate(l.p5Kbps) : '—'}</b><span>5% client chậm nhất</span></div>
        <div><b>${l.fps.toFixed(0)}</b><span>frame/s tổng</span></div>
        <div><b>${l.ttfbP95 ? fmtMs(l.ttfbP95) : '—'}</b><span>TTFB p95</span></div>
        <div><b class="${l.lossPct >= 1 ? 'c-bad' : ''}">${fmtPct(l.lossPct)}</b><span>mất gói</span></div>
        <div><b class="${l.fail ? 'c-bad' : ''}">${l.ok}/${l.fail}</b><span>ok / lỗi</span></div>
      </div></div>`;
}

/* ------------------------------------------------------------------ camera detail */

async function dashCamDetail(id) {
  let r;
  try { r = await api('/monitor/cameras/' + id); } catch (e) { toast(e.message, 'err'); return; }
  const c = r.camera;
  const m = modal({
    title: `${c.name} · ${c.slug}`,
    body: `<div id="cdKpi" class="mini"></div><div id="cdChart"></div><div id="cdSess"></div>
      <div class="row"><button class="sm" id="cdAdd">＋ Thêm vào biểu đồ chính</button><span class="muted small">Chart cập nhật mỗi giây khi cửa sổ này mở.</span></div>`,
    footer: '<button data-close>Đóng</button>',
  });
  const ch = new TimeChart(m.q('#cdChart'), { height: 220, fmt: fmtRate, windowSec: 300, emptyMax: 1000,
    series: [{ key: 'out', label: 'Ra (viewers)', color: '#38bdf8', width: 2 }, { key: 'in', label: 'Vào (từ source)', color: '#34d399' }] });
  try {
    const s = await api(`/monitor/series?range=300&cams=${encodeURIComponent(c.slug)}`);
    ch.setData(s.global.ts, { out: s.cams[c.slug].out, in: s.cams[c.slug].in });
  } catch { /* chart starts empty */ }
  const paint = (cur, sessions) => {
    m.q('#cdKpi').innerHTML = `
      <div><b>${fmtRate(cur.outKbps)}</b><span>ra hiện tại</span></div>
      <div><b>${cur.readers}</b><span>viewers${protoTip(cur) ? ' (' + esc(protoTip(cur)) + ')' : ''}</span></div>
      <div><b>${cur.readers ? fmtRate(cur.perReaderKbps) : '—'}</b><span>TB / viewer</span></div>
      <div><b>${fmtRate(cur.peakOutKbps)}</b><span>đỉnh</span></div>
      <div><b class="${cur.lossPct >= 0.5 ? 'c-warn' : ''}">${fmtPct(cur.lossTotalPct)}</b><span>mất gói tổng · bị loại ${fmt(cur.discarded)}</span></div>
      <div><b>${resTxt(cur)}</b><span>${esc((cur.tracks || []).join(' + ') || '—')}</span></div>`;
    m.q('#cdSess').innerHTML = sessions.length ? `<div class="tablewrap" style="max-height:240px"><table><thead><tr><th>Client</th><th>Agent</th><th>Giao thức</th><th class="num">Tốc độ</th><th>Mất gói</th><th class="num">Đã gửi</th><th>Tuổi</th></tr></thead><tbody>
      ${sessions.map((x) => `<tr><td class="slug">${esc(x.remote)}${x.internal ? ' <span class="muted">(proxy)</span>' : ''}</td><td>${esc(x.agent || '—')}</td><td>${esc(x.transport)}</td><td class="num">${fmtRate(x.kbps)}</td>
        <td>${fmtPct(x.lossPct)}${x.discarded ? ` <span class="c-warn small">(${x.discarded} bị loại)</span>` : ''}</td><td class="num">${fmtBytes(x.bytes)}</td><td class="muted small">${ago(x.created)}</td></tr>`).join('')}</tbody></table></div>`
      : '<div class="muted small">Không có client RTSP nào đang đọc (HLS/WebRTC không hiển thị theo từng session).</div>';
  };
  paint(c, r.sessions || []);
  m.q('#cdAdd').addEventListener('click', () => {
    const d = state.dash;
    if (!d) return;
    if (!d.prefs.sel.includes(c.slug)) toggleCam(d, c.slug);
    toast('Đã thêm ' + c.slug + ' vào biểu đồ', 'ok');
  });
  const timer = setInterval(async () => {
    if (!document.body.contains(m.q('#cdKpi'))) { clearInterval(timer); ch.destroy(); return; }
    try {
      const x = await api('/monitor/cameras/' + id);
      ch.push(Math.floor(Date.now() / 1000), { out: x.camera.outKbps, in: x.camera.inKbps });
      paint(x.camera, x.sessions || []);
    } catch { /* ignore */ }
  }, 1000);
}

/* ================================================================== load test page */

const LT_METRICS = [
  ['handshake', 'Handshake (connect→PLAY)', fmtMs], ['describe', 'DESCRIBE', fmtMs], ['ttfb', 'TTFB (PLAY→gói RTP đầu)', fmtMs],
  ['start', 'Thời gian chờ hình (connect→gói đầu)', fmtMs], ['bitrate', 'Bitrate mỗi client', fmtRate], ['fps', 'FPS mỗi client', (v) => v.toFixed(1)],
  ['jitter', 'Jitter RTP', fmtMs], ['maxGap', 'Khoảng lặng dài nhất', fmtMs], ['delay', 'Độ trễ relay (RTCP SR)', fmtMs],
];

async function pageLoadtest() {
  const [camsOut, runs] = await Promise.all([api('/monitor/cameras?sort=name'), api('/loadtest')]);
  const cams = camsOut.items.filter((c) => c.enabled);
  const groups = [...new Set(cams.map((c) => c.group).filter(Boolean))];
  const sources = [...new Map(cams.map((c) => [c.sourceId, c.sourceName])).entries()];
  const lt = state.lt = { charts: {}, timer: null, runId: '', cams, sel: new Set(), target: 'all' };
  $('#view').innerHTML = `
    <div class="panel">
      <div class="panel-head"><h2>Load test — mô phỏng N client xem camera</h2><span class="spacer"></span><span class="muted small">${cams.length} camera đang bật</span></div>
      <div class="panel-body">
        <form id="ltForm" class="formgrid3">
          <label>Chế độ<select name="mode" id="ltMode">
            <option value="stream">stream — kéo media thật (đo băng thông, TTFB, fps, mất gói…)</option>
            <option value="describe">describe — chỉ RTSP DESCRIBE (đo khả năng kết nối/auth)</option>
          </select></label>
          <label>Số client<input name="count" type="number" value="50" min="1" max="2000" /></label>
          <label class="lt-s">Thời lượng (giây)<input name="durationSec" type="number" value="30" min="3" max="3600" /></label>
          <label class="lt-s">Ramp-up (giây)<input name="rampSec" type="number" value="5" min="0" max="600" /></label>
          <label class="lt-d">Concurrency<input name="concurrency" type="number" value="20" min="1" max="200" /></label>
          <label>Timeout (ms)<input name="timeoutMs" type="number" value="8000" min="500" /></label>
          <label class="full">Camera mục tiêu (client xoay vòng đều qua các camera này)
            <select id="ltTarget"><option value="all">Tất cả camera đang bật</option>
              ${groups.length ? '<option value="group">Theo group…</option>' : ''}${sources.length > 1 ? '<option value="source">Theo source…</option>' : ''}<option value="pick">Chọn từng camera…</option></select></label>
          <div class="full" id="ltTargetBox"></div>
        </form>
        <div class="row" style="margin-top:10px">
          <button class="primary" id="ltRun">▶ Chạy</button><button class="danger hidden" id="ltStop">■ Dừng</button>
          <span class="muted small" id="ltHint"></span>
        </div>
        <div class="hint" style="margin-top:8px">Client chạy ngay trong control plane nên dùng CPU/NIC của <b>cùng máy</b> với MediaMTX; với test lớn hãy theo dõi CPU ở Dashboard. Stream chỉ hỗ trợ RTSP over TCP (interleaved).</div>
      </div>
    </div>
    <div id="ltResult"></div>
    <div class="panel" id="ltHistory"></div>`;

  const hint = () => {
    const f = new FormData($('#ltForm'));
    const stream = f.get('mode') === 'stream';
    document.querySelectorAll('.lt-s').forEach((e) => e.classList.toggle('hidden', !stream));
    document.querySelectorAll('.lt-d').forEach((e) => e.classList.toggle('hidden', stream));
    const n = Number(f.get('count'));
    $('#ltHint').textContent = stream ? `${n} client × ${f.get('durationSec')}s, vào dần trong ${f.get('rampSec')}s` : `${n} lượt DESCRIBE, ${f.get('concurrency')} song song`;
  };
  $('#ltForm').addEventListener('input', hint);
  $('#ltTarget').addEventListener('change', (e) => { lt.target = e.target.value; lt.sel.clear(); paintTargetBox(lt, groups, sources); });
  paintTargetBox(lt, groups, sources);
  hint();
  $('#ltRun').addEventListener('click', (e) => { e.preventDefault(); ltStart(lt); });
  $('#ltStop').addEventListener('click', async () => {
    try { await api(`/loadtest/${lt.runId}/stop`, { method: 'POST' }); toast('Đang dừng…'); } catch (err) { toast(err.message, 'err'); }
  });
  $('#ltHistory').addEventListener('click', (e) => {
    const r = e.target.closest('[data-run]');
    if (r) ltWatch(lt, r.dataset.run);
  });
  paintHistory(runs.items);
  const running = runs.items.find((r) => r.status === 'running') || runs.items[0];
  if (running) ltWatch(lt, running.id);
}

function paintTargetBox(lt, groups, sources) {
  const box = $('#ltTargetBox');
  if (lt.target === 'all') { box.innerHTML = ''; return; }
  if (lt.target === 'group') box.innerHTML = `<select id="ltGroup">${groups.map((g) => `<option>${esc(g)}</option>`).join('')}</select>`;
  if (lt.target === 'source') box.innerHTML = `<select id="ltSource">${sources.map(([id, n]) => `<option value="${esc(id)}">${esc(n)}</option>`).join('')}</select>`;
  if (lt.target === 'pick') {
    box.innerHTML = `<input id="ltPickQ" placeholder="Tìm camera…" /><div class="plist" id="ltPickList"></div><div class="muted small" id="ltPickN"></div>`;
    const paint = () => {
      const q = ($('#ltPickQ').value || '').toLowerCase();
      $('#ltPickList').innerHTML = lt.cams.filter((c) => !q || (c.slug + c.name).toLowerCase().includes(q)).slice(0, 200)
        .map((c) => `<label class="pitem"><input type="checkbox" data-id="${esc(c.id)}" ${lt.sel.has(c.id) ? 'checked' : ''} /> <span class="slug">${esc(c.slug)}</span> <span class="muted small">${esc(c.name)}</span></label>`).join('');
      $('#ltPickN').textContent = `${lt.sel.size} camera được chọn`;
    };
    $('#ltPickQ').addEventListener('input', paint);
    $('#ltPickList').addEventListener('change', (e) => { const id = e.target.dataset.id; e.target.checked ? lt.sel.add(id) : lt.sel.delete(id); $('#ltPickN').textContent = `${lt.sel.size} camera được chọn`; });
    paint();
  }
}

async function ltStart(lt) {
  const f = new FormData($('#ltForm'));
  const body = {
    mode: f.get('mode'), count: Number(f.get('count')), concurrency: Number(f.get('concurrency')), timeoutMs: Number(f.get('timeoutMs')),
    durationSec: Number(f.get('durationSec')), rampSec: Number(f.get('rampSec')),
  };
  if (lt.target === 'group') body.group = $('#ltGroup').value;
  if (lt.target === 'source') body.sourceId = $('#ltSource').value;
  if (lt.target === 'pick') {
    if (!lt.sel.size) { toast('Chọn ít nhất một camera', 'err'); return; }
    body.cameraIds = [...lt.sel];
  }
  try {
    const run = await api('/loadtest', { method: 'POST', body });
    toast('Đã bắt đầu ' + run.id, 'ok');
    ltWatch(lt, run.id);
  } catch (err) { toast(err.message, 'err'); }
}

function ltBuildResult(lt) {
  Object.values(lt.charts).forEach((c) => c.destroy());
  lt.charts = {};
  $('#ltResult').innerHTML = `
    <div class="panel"><div class="panel-head"><h2 id="ltTitle">Kết quả</h2><span class="muted small" id="ltId"></span><span class="spacer"></span>
      <button class="sm" id="ltJson">⬇ JSON</button><button class="sm" id="ltCsv">⬇ CSV</button></div>
      <div class="panel-body" style="display:grid;gap:14px">
        <div class="bar"><i id="ltBar" style="width:0"></i></div>
        <div id="ltVerdict"></div>
        <div class="kpis" id="ltKpis"></div>
        <div class="chartgrid3">
          <div><h3 class="ph">Thông lượng tổng (Mbps)</h3><div id="lcTp"></div></div>
          <div><h3 class="ph">Client</h3><div id="lcCl"></div></div>
          <div><h3 class="ph">Tốc độ mỗi client</h3><div id="lcPc"></div></div>
        </div>
        <div id="ltDist"></div><div id="ltErr"></div><div id="ltCam"></div>
      </div></div>`;
  const base = { height: 170, windowSec: 60, emptyMax: 1 };
  lt.charts.tp = new TimeChart($('#lcTp'), { ...base, fmt: (v) => v.toFixed(v >= 100 ? 0 : 1) + ' Mbps', series: [{ key: 'mbps', label: 'Thông lượng', color: '#38bdf8', width: 2 }] });
  lt.charts.cl = new TimeChart($('#lcCl'), { ...base, fmt: fmtInt, step: 'step', series: [{ key: 'active', label: 'Đang nhận media', color: '#34d399' }, { key: 'fail', label: 'Lỗi (cộng dồn)', color: '#fb7185' }] });
  lt.charts.pc = new TimeChart($('#lcPc'), { ...base, fmt: fmtRate, emptyMax: 100, series: [{ key: 'avg', label: 'TB / client', color: '#a78bfa' }, { key: 'p5', label: '5% chậm nhất', color: '#f59e0b' }] });
  $('#ltJson').onclick = () => lt.last && download(`${lt.last.id}.json`, JSON.stringify(lt.last, null, 2), 'application/json');
  $('#ltCsv').onclick = () => {
    if (!lt.last) return;
    const rows = ['t,active,mbps,fps,avgKbps,p5Kbps,lost,stalls,failCumulative', ...(lt.last.series || []).map((p) => [p.t, p.active, p.mbps.toFixed(3), p.fps, p.avgKbps.toFixed(1), p.p5Kbps.toFixed(1), p.lost, p.stalls, p.fail].join(','))];
    download(`${lt.last.id}-series.csv`, rows.join('\n'), 'text/csv');
  };
}

function download(name, text, type) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], { type }));
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 2000);
}

function ltWatch(lt, id) {
  if (lt.timer) clearTimeout(lt.timer);
  lt.runId = id;
  ltBuildResult(lt);
  const tick = async () => {
    if (state.lt !== lt || lt.runId !== id) return;
    try {
      const r = await api('/loadtest/' + id);
      lt.last = r;
      ltPaint(lt, r);
      if (r.status === 'running') lt.timer = setTimeout(tick, 1000);
      else { const h = await api('/loadtest'); paintHistory(h.items); }
    } catch (err) { toast(err.message, 'err'); }
  };
  tick();
}

function verdict(r) {
  const problems = [];
  let level = 0;
  const bump = (l, msg) => { level = Math.max(level, l); problems.push((l === 2 ? '✖ ' : '⚠ ') + msg); };
  const total = r.ok + r.fail;
  if (r.fail > 0) bump(r.fail / Math.max(1, total) > 0.05 ? 2 : 1, `${r.fail}/${total} client lỗi`);
  if (r.mode === 'describe') {
    if (r.p95Ms > 3000) bump(2, `DESCRIBE p95 ${r.p95Ms} ms`); else if (r.p95Ms > 1000) bump(1, `DESCRIBE p95 ${r.p95Ms} ms`);
  } else {
    if (r.lossPct > 1) bump(2, `mất gói ${r.lossPct.toFixed(2)}%`); else if (r.lossPct > 0.1) bump(1, `mất gói ${r.lossPct.toFixed(2)}%`);
    if (r.stalledClients > 0) bump(r.stalledClients / Math.max(1, total) > 0.05 ? 2 : 1, `${r.stalledClients} client bị đứng hình > 1s`);
    if (r.dropped > 0) bump(2, `${r.dropped} client bị ngắt kết nối giữa chừng`);
    if (r.ttfb.p95 > 3000) bump(2, `TTFB p95 ${fmtMs(r.ttfb.p95)}`); else if (r.ttfb.p95 > 1000) bump(1, `TTFB p95 ${fmtMs(r.ttfb.p95)}`);
    if (r.bitrate.n > 3 && r.bitrate.min < r.bitrate.p50 * 0.5) bump(1, `có client chỉ nhận ${fmtRate(r.bitrate.min)} (trung vị ${fmtRate(r.bitrate.p50)})`);
    const lastCl = (r.series || []).slice(-1)[0];
    if (lastCl && lastCl.avgKbps && lastCl.p5Kbps < lastCl.avgKbps * 0.5 && r.status === 'running') bump(1, '5% client chậm nhất < 50% trung bình');
  }
  return { level, problems };
}

function ltPaint(lt, r) {
  const stream = r.mode === 'stream';
  const running = r.status === 'running';
  $('#ltTitle').innerHTML = `Kết quả ${running ? '<span class="spinner"></span>' : r.stopped ? '(đã dừng)' : ''}`;
  $('#ltId').textContent = `${r.id} · ${r.mode} · ${r.count} client / ${r.cameras} camera`;
  $('#ltStop').classList.toggle('hidden', !running);
  $('#ltRun').disabled = running;
  const pct = stream && r.durationSec ? Math.min(100, (r.elapsedMs / 1000 / (r.durationSec + r.rampSec)) * 100) : r.count ? (r.done / r.count) * 100 : 0;
  $('#ltBar').style.width = (running ? pct : 100).toFixed(0) + '%';

  const v = verdict(r);
  $('#ltVerdict').innerHTML = running && !r.done ? '' : `<div class="verdict v${v.level}"><b>${v.level === 0 ? '✔ ĐẠT' : v.level === 1 ? '⚠ CẦN CHÚ Ý' : '✖ KHÔNG ĐẠT'}</b>${running ? ' (tạm tính)' : ''}
    ${v.problems.length ? '<ul>' + v.problems.map((p) => `<li>${esc(p)}</li>`).join('') + '</ul>' : ' — không phát hiện vấn đề.'}</div>`;

  const last = (r.series || []).slice(-1)[0] || {};
  const k = [];
  if (stream) {
    k.push(['Client', `${r.active}/${r.count}`, `đang nhận media · xong ${r.done} · ok ${r.ok} · lỗi ${r.fail}${r.dropped ? ' · rớt ' + r.dropped : ''}`, r.fail ? 'bad' : '']);
    k.push(['Thông lượng', (last.mbps || 0).toFixed(1) + ' Mbps', `TB ${r.avgMbps.toFixed(1)} · đỉnh ${r.peakMbps.toFixed(1)} Mbps`, '']);
    k.push(['Mỗi client', last.avgKbps ? fmtRate(last.avgKbps) : '—', `5% chậm nhất ${last.p5Kbps ? fmtRate(last.p5Kbps) : '—'}${r.bitrate.n ? ' · cuối: ' + fmtRate(r.bitrate.min) + '–' + fmtRate(r.bitrate.max) : ''}`, last.avgKbps && last.p5Kbps < last.avgKbps * 0.5 ? 'warn' : '']);
    k.push(['TTFB p50 / p95', `${fmtMs(r.ttfb.p50)} / ${fmtMs(r.ttfb.p95)}`, `max ${fmtMs(r.ttfb.max)} · chờ hình p95 ${fmtMs(r.start.p95)}`, tone(r.ttfb.p95, 1000, 3000)]);
    k.push(['Frame/s', (last.fps || 0).toFixed(0), r.fps.n ? `mỗi client p50 ${r.fps.p50.toFixed(1)} · min ${r.fps.min.toFixed(1)}` : `${fmt(r.totalFrames)} frame`, '']);
    k.push(['Mất gói', fmtPct(r.lossPct), `${fmt(r.totalLost)} gói · ${r.stalls} lần đứng > 1s (${r.stalledClients} client)`, tone(r.lossPct, 0.1, 1)]);
    k.push(['Jitter / trễ relay', r.jitter.n ? `${fmtMs(r.jitter.p95)} p95` : '—', r.delay.n ? `relay TB ${fmtMs(r.delay.avg)} · max ${fmtMs(r.delay.max)}` : 'relay: chưa có RTCP SR', '']);
    k.push(['Dữ liệu', fmtBytes(r.totalBytes), `${fmt(r.totalPackets)} gói RTP`, '']);
  } else {
    k.push(['Hoàn thành', `${r.done}/${r.count}`, `ok ${r.ok} · lỗi ${r.fail} · ${r.active} đang chạy`, r.fail ? 'bad' : '']);
    k.push(['DESCRIBE p50 / p95', `${r.p50Ms} / ${r.p95Ms} ms`, `avg ${r.avgMs} · max ${r.maxMs} ms`, tone(r.p95Ms, 1000, 3000)]);
    k.push(['Thời gian', ((r.durationMs || r.elapsedMs) / 1000).toFixed(1) + ' s', `${r.concurrency} song song`, '']);
  }
  $('#ltKpis').innerHTML = k.map(([t, val, sub, cls]) => `<div class="kpi ${cls || ''}"><h3>${esc(t)}</h3><div class="kv">${esc(val)}</div><div class="ks">${esc(sub)}</div></div>`).join('');

  const s = r.series || [];
  const ts = s.map((p) => p.t);
  const win = Math.max(30, (s.length ? s[s.length - 1].t : 0) + 2);
  for (const c of Object.values(lt.charts)) c.setWindow(win);
  lt.charts.tp.setData(ts, { mbps: s.map((p) => p.mbps) });
  lt.charts.cl.setData(ts, { active: s.map((p) => p.active), fail: s.map((p) => p.fail) });
  lt.charts.pc.setData(ts, { avg: s.map((p) => p.avgKbps), p5: s.map((p) => p.p5Kbps) });

  const fmtD = (f) => (d) => (d.n ? [f(d.min), f(d.avg), f(d.p50), f(d.p95), f(d.p99), f(d.max)] : null);
  const rows = (stream ? LT_METRICS : LT_METRICS.slice(0, 1)).map(([key, label, f]) => ({ label, d: r[key], cells: fmtD(f)(r[key] || { n: 0 }) })).filter((x) => x.cells);
  $('#ltDist').innerHTML = rows.length ? `<div><h3 class="ph">Phân bố theo client ${running ? '<span class="muted small">(TTFB/handshake cập nhật trực tiếp, còn lại chốt khi client kết thúc)</span>' : ''}</h3>
    <div class="tablewrap"><table><thead><tr><th>Chỉ số</th><th class="num">n</th><th class="num">min</th><th class="num">avg</th><th class="num">p50</th><th class="num">p95</th><th class="num">p99</th><th class="num">max</th></tr></thead><tbody>
    ${rows.map((x) => `<tr><td>${esc(x.label)}</td><td class="num">${x.d.n}</td>${x.cells.map((c) => `<td class="num">${esc(c)}</td>`).join('')}</tr>`).join('')}</tbody></table></div></div>` : '';

  const errs = Object.entries(r.errors || {}).sort((a, b) => b[1] - a[1]).slice(0, 10);
  $('#ltErr').innerHTML = errs.length ? `<div><h3 class="ph">Lỗi</h3><pre>${esc(errs.map(([m, n]) => n + '× ' + m).join('\n'))}</pre></div>` : '';
  $('#ltCam').innerHTML = stream && (r.perCamera || []).length ? `<details><summary>Kết quả theo camera (${r.perCamera.length})</summary><div class="tablewrap" style="max-height:320px"><table><thead><tr><th>Camera</th><th class="num">Client</th><th class="num">OK</th><th class="num">Lỗi</th><th class="num">TB bitrate</th><th class="num">TTFB TB</th><th class="num">Mất gói</th></tr></thead><tbody>
    ${r.perCamera.map((c) => `<tr><td class="slug">${esc(c.slug)}</td><td class="num">${c.clients}</td><td class="num">${c.ok}</td><td class="num ${c.fail ? 'c-bad' : ''}">${c.fail}</td><td class="num">${c.avgKbps ? fmtRate(c.avgKbps) : '—'}</td><td class="num">${c.avgTtfbMs ? fmtMs(c.avgTtfbMs) : '—'}</td><td class="num">${fmtPct(c.lossPct)}</td></tr>`).join('')}</tbody></table></div></details>` : '';
}

function paintHistory(items) {
  const el = $('#ltHistory');
  if (!el) return;
  el.innerHTML = `<div class="panel-head"><h2>Lịch sử (bộ nhớ, ${items.length} lần gần nhất)</h2></div>` + (items.length ? `<div class="tablewrap"><table><thead><tr><th>Thời gian</th><th>Chế độ</th><th class="num">Client</th><th class="num">OK/Lỗi</th><th class="num">Mbps đỉnh</th><th class="num">TTFB p95</th><th class="num">Mất gói</th><th></th></tr></thead><tbody>
    ${items.map((r) => `<tr><td class="muted">${esc(new Date(r.createdAt).toLocaleString())}</td><td>${esc(r.mode)}${r.status === 'running' ? ' <span class="spinner"></span>' : r.stopped ? ' (dừng)' : ''}</td><td class="num">${r.count}</td>
      <td class="num">${r.ok}/<span class="${r.fail ? 'c-bad' : ''}">${r.fail}</span></td><td class="num">${r.mode === 'stream' ? r.peakMbps.toFixed(1) : '—'}</td><td class="num">${r.mode === 'stream' ? fmtMs(r.ttfb.p95) : r.p95Ms + ' ms'}</td><td class="num">${r.mode === 'stream' ? fmtPct(r.lossPct) : '—'}</td>
      <td><button class="sm" data-run="${esc(r.id)}">Xem</button></td></tr>`).join('')}</tbody></table></div>` : '<div class="panel-body muted">Chưa có lần chạy nào.</div>');
}
