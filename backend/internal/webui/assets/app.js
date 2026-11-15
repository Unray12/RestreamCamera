/* RestreamCamera — embedded SPA (no build step). */
'use strict';

const $ = (s) => document.querySelector(s);
const state = {
  token: localStorage.getItem('rc_token') || '',
  page: (location.hash || '#dashboard').slice(1),
  overview: null,
  stats: null,
  sources: [],
  cam: { q: '', st: '', group: '', page: 1, pageSize: 25, total: 0, items: [], sel: new Set() },
  es: null,
  timer: null,
};

const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const fmt = (n) => new Intl.NumberFormat('vi-VN').format(Math.round(n || 0));
const kbps = (n) => (n >= 1000 ? (n / 1000).toFixed(2) + ' Mbps' : Math.round(n || 0) + ' kbps');
const ago = (t) => {
  if (!t) return '—';
  const d = (Date.now() - new Date(t).getTime()) / 1000;
  if (d < 60) return Math.max(0, Math.round(d)) + 's';
  if (d < 3600) return Math.round(d / 60) + 'm';
  if (d < 86400) return Math.round(d / 3600) + 'h';
  return Math.round(d / 86400) + 'd';
};

async function api(path, opts = {}) {
  const { method = 'GET', body } = opts;
  const res = await fetch('/api/v1' + path, {
    method,
    headers: {
      ...(body ? { 'Content-Type': 'application/json' } : {}),
      ...(state.token ? { Authorization: 'Bearer ' + state.token } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (res.status === 401 && !path.startsWith('/auth') && !path.startsWith('/mediamtx')) {
    logout();
    throw new Error('Phiên đăng nhập hết hạn');
  }
  if (!res.ok) throw new Error((data && (data.detail || data.title)) || res.statusText);
  return data;
}

function toast(msg, kind = '') {
  const el = document.createElement('div');
  el.className = 'toast ' + kind;
  el.textContent = msg;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), 4200);
}

function copy(text) {
  navigator.clipboard.writeText(text).then(
    () => toast('Đã copy: ' + text.slice(0, 64), 'ok'),
    () => toast('Không copy được (clipboard bị chặn)', 'err'),
  );
}

/* ------------------------------------------------------------------ auth */

function showLogin() {
  $('#login').classList.remove('hidden');
  $('#app').classList.add('hidden');
}

function logout() {
  state.token = '';
  localStorage.removeItem('rc_token');
  if (state.es) { state.es.close(); state.es = null; }
  if (state.timer) { clearInterval(state.timer); state.timer = null; }
  showLogin();
}

async function onLogin(e) {
  e.preventDefault();
  const f = new FormData(e.target);
  $('#loginError').textContent = '';
  try {
    const out = await api('/auth/login', {
      method: 'POST',
      body: { username: f.get('username'), password: f.get('password') },
    });
    state.token = out.token;
    localStorage.setItem('rc_token', out.token);
    $('#login').classList.add('hidden');
    $('#app').classList.remove('hidden');
    await start();
  } catch (err) {
    $('#loginError').textContent = err.message;
  }
}

/* ------------------------------------------------------------------ shell */

function renderTopMetrics() {
  const o = state.overview || {};
  const s = state.stats || {};
  $('#topMetrics').innerHTML = `
    <div class="tm"><b id="kpi-readers">${fmt(s.readers || 0)}</b><span>readers</span></div>
    <div class="tm"><b id="kpi-out">${kbps(s.outKbps || 0)}</b><span>egress</span></div>
    <div class="tm"><b id="kpi-ready">${fmt(o.camerasReady || 0)}/${fmt(o.camerasTotal || 0)}</b><span>ready</span></div>`;
}

async function start() {
  await api('/me');
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
  try {
    const sys = await api('/system');
    $('#ver').textContent = 'v' + sys.version;
    setMediaBadge(sys.mediaOnline);
  } catch { /* ignore */ }
  await loadOverview();
  connectSSE();
  await navigate(state.page || 'dashboard');
}

function setMediaBadge(online) {
  const b = $('#mediaBadge');
  if (!b) return;
  b.className = 'badge ' + (online ? 'ok' : 'bad');
  b.textContent = 'media: ' + (online ? 'online' : 'offline');
}

async function loadOverview() {
  try {
    const out = await api('/overview');
    state.overview = out.overview;
    state.stats = out.stats;
    renderTopMetrics();
  } catch (e) { /* offline */ }
}

function connectSSE() {
  if (state.es) state.es.close();
  const es = new EventSource('/api/v1/monitor/stream?token=' + encodeURIComponent(state.token));
  es.addEventListener('stats', (ev) => {
    try {
      const d = JSON.parse(ev.data);
      state.overview = d.overview;
      state.stats = d.stats;
      renderTopMetrics();
      const r = $('#mon-readers'); if (r) r.textContent = fmt(d.stats.readers);
      const o = $('#mon-out'); if (o) o.textContent = kbps(d.stats.outKbps);
      const rd = $('#dash-readers'); if (rd) rd.textContent = fmt(d.stats.readers);
      const ob = $('#dash-out'); if (ob) ob.textContent = kbps(d.stats.outKbps);
      const rdy = $('#dash-ready'); if (rdy) rdy.textContent = fmt(d.overview.camerasReady);
    } catch { /* ignore */ }
  });
  state.es = es;
}

window.addEventListener('hashchange', () => {
  const p = location.hash.slice(1) || 'dashboard';
  if (p !== state.page) navigate(p);
});

async function navigate(page) {
  state.page = page;
  if (state.timer) { clearInterval(state.timer); state.timer = null; }
  document.querySelectorAll('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.page === page));
  const titles = { dashboard: 'Dashboard', sources: 'Sources', cameras: 'Virtual cameras', monitor: 'Monitor', loadtest: 'Load test', events: 'Events', system: 'System' };
  $('#pageTitle').textContent = titles[page] || page;
  $('#view').innerHTML = '<div class="panel"><div class="panel-body"><span class="spinner"></span> Đang tải…</div></div>';
  try {
    if (page === 'dashboard') await pageDashboard();
    else if (page === 'sources') await pageSources();
    else if (page === 'cameras') await pageCameras();
    else if (page === 'monitor') await pageMonitor();
    else if (page === 'loadtest') await pageLoadtest();
    else if (page === 'events') await pageEvents();
    else if (page === 'system') await pageSystem();
    else await pageDashboard();
  } catch (err) {
    $('#view').innerHTML = `<div class="panel"><div class="panel-body error">${esc(err.message)}</div></div>`;
  }
}

/* ------------------------------------------------------------------ dashboard */

async function pageDashboard() {
  const [ov, samples, events, cams] = await Promise.all([
    api('/overview'),
    api('/samples?limit=180'),
    api('/events?limit=8'),
    api('/cameras?pageSize=200&page=1'),
  ]);
  state.overview = ov.overview;
  state.stats = ov.stats;
  renderTopMetrics();
  const top = cams.items
    .filter((c) => c.runtime.readers > 0)
    .sort((a, b) => b.runtime.readers - a.runtime.readers)
    .slice(0, 8);
  const o = ov.overview;
  $('#view').innerHTML = `
    <div class="cards">
      <div class="card"><h3>Camera ảo</h3><div class="value">${fmt(o.camerasTotal)}</div><div class="sub">${fmt(o.camerasReady)} ready · ${fmt(o.camerasIdle || 0)} idle · ${fmt(o.camerasOffline)} offline</div></div>
      <div class="card"><h3>Readers</h3><div class="value" id="dash-readers">${fmt(ov.stats.readers)}</div><div class="sub">client đang kết nối</div></div>
      <div class="card"><h3>Egress</h3><div class="value" id="dash-out">${kbps(ov.stats.outKbps)}</div><div class="sub">ingest ${kbps(ov.stats.inKbps)}</div></div>
      <div class="card"><h3>Sources</h3><div class="value">${fmt(o.sourcesTotal)}</div><div class="sub">${fmt(o.sourcesOnline)} online</div></div>
    </div>

    <div class="grid2">
      <div class="panel">
        <div class="panel-head"><h2>Readers theo thời gian</h2><span class="spacer"></span><span class="muted small">1 điểm / phút</span></div>
        <div class="panel-body"><canvas id="spark" class="spark"></canvas></div>
      </div>
      <div class="panel">
        <div class="panel-head"><h2>Top cameras</h2></div>
        <div class="panel-body">
          ${top.length ? `<div class="tablewrap"><table><thead><tr><th>Slug</th><th class="num">Readers</th><th class="num">Egress</th><th>State</th></tr></thead><tbody>
            ${top.map((c) => `<tr><td class="slug">${esc(c.slug)}</td><td class="num">${c.runtime.readers}</td><td class="num">${kbps(c.runtime.outKbps)}</td><td><span class="state ${esc(c.runtime.state)}">${esc(c.runtime.state)}</span></td></tr>`).join('')}
          </tbody></table></div>` : '<div class="muted">Chưa có client nào. Tạo camera ảo rồi mở bằng ffplay/VLC.</div>'}
        </div>
      </div>
    </div>

    <div class="panel">
      <div class="panel-head"><h2>Sự kiện gần đây</h2><span class="spacer"></span><a href="#events">Xem tất cả →</a></div>
      <div class="panel-body">${eventsTable(events.items)}</div>
    </div>`;
  drawSpark(samples.items);
}

function drawSpark(samples) {
  const cv = $('#spark');
  if (!cv) return;
  const dpr = window.devicePixelRatio || 1;
  const w = cv.clientWidth || 600;
  const h = 120;
  cv.width = w * dpr;
  cv.height = h * dpr;
  const ctx = cv.getContext('2d');
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, w, h);
  if (!samples.length) {
    ctx.fillStyle = '#64748b';
    ctx.font = '12px sans-serif';
    ctx.fillText('Chưa có dữ liệu (sample mỗi phút)', 12, h / 2);
    return;
  }
  const vals = samples.map((s) => s.readers);
  const max = Math.max(1, ...vals);
  const stepX = w / Math.max(1, vals.length - 1);
  ctx.beginPath();
  vals.forEach((v, i) => {
    const x = i * stepX;
    const y = h - 10 - (v / max) * (h - 26);
    i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
  });
  ctx.strokeStyle = '#38bdf8';
  ctx.lineWidth = 2;
  ctx.stroke();
  ctx.lineTo(w, h);
  ctx.lineTo(0, h);
  ctx.closePath();
  const g = ctx.createLinearGradient(0, 0, 0, h);
  g.addColorStop(0, 'rgba(56,189,248,.28)');
  g.addColorStop(1, 'rgba(56,189,248,0)');
  ctx.fillStyle = g;
  ctx.fill();
  ctx.fillStyle = '#8b9bb4';
  ctx.font = '11px sans-serif';
  ctx.fillText(`max ${max} readers`, 8, 14);
}

function eventsTable(items) {
  if (!items || !items.length) return '<div class="muted">Chưa có sự kiện.</div>';
  return `<div class="tablewrap"><table><thead><tr><th>Thời gian</th><th>Level</th><th>Loại</th><th>Đối tượng</th><th>Nội dung</th></tr></thead><tbody>
    ${items.map((e) => `<tr><td class="muted">${esc(new Date(e.ts).toLocaleString())}</td>
      <td><span class="state ${e.level === 'warn' ? 'disabled' : e.level === 'error' ? 'offline' : 'ready'}">${esc(e.level)}</span></td>
      <td>${esc(e.category)}</td><td>${esc(e.subject || '—')}</td><td>${esc(e.message)}</td></tr>`).join('')}
  </tbody></table></div>`;
}

/* ------------------------------------------------------------------ sources */

async function pageSources() {
  const out = await api('/sources');
  state.sources = out.items;
  $('#view').innerHTML = `
    <div class="row">
      <button class="primary" data-act="source-new">+ Thêm source</button>
      <span class="muted small">1 camera thật → N camera ảo. Source kiểu <b>publisher</b> chờ ffmpeg/OBS đẩy vào.</span>
      <span class="spacer"></span>
      <button data-act="refresh">↻</button>
    </div>
    <div class="panel">
      <div class="tablewrap"><table>
        <thead><tr><th>Tên</th><th>Kiểu</th><th>URL</th><th>Path</th><th>Trạng thái</th><th>Cập nhật</th><th></th></tr></thead>
        <tbody>
        ${out.items.map((s) => `<tr>
          <td><b>${esc(s.name)}</b><div class="muted small">${esc(s.id)}</div></td>
          <td>${esc(s.kind)}${s.kind === 'pull' ? ` · ${esc(s.transport)}` : ''}</td>
          <td class="slug">${esc(s.urlMasked || '—')}</td>
          <td class="slug">${esc(s.path)}</td>
          <td><span class="state ${esc(s.status)}">${esc(s.status)}</span>${s.lastError ? `<div class="muted small">${esc(s.lastError)}</div>` : ''}</td>
          <td class="muted small">${ago(s.updatedAt)} trước</td>
          <td class="row">
            ${s.kind === 'pull' ? `<button class="sm" data-act="source-test" data-id="${s.id}">Test</button>` : ''}
            <button class="sm" data-act="source-toggle" data-id="${s.id}" data-enabled="${s.enabled}">${s.enabled ? 'Tắt' : 'Bật'}</button>
            <button class="sm danger" data-act="source-del" data-id="${s.id}" data-name="${esc(s.name)}">Xoá</button>
          </td>
        </tr>`).join('') || '<tr><td colspan="7" class="muted">Chưa có source. Thêm camera thật hoặc tạo publisher path.</td></tr>'}
        </tbody>
      </table></div>
    </div>`;
}

function sourceModal() {
  const m = modal({
    title: 'Thêm source',
    body: `<form id="srcForm" class="formgrid">
      <label class="full">Tên<input name="name" placeholder="Camera cổng chính" required /></label>
      <label>Kiểu<select name="kind">
        <option value="pull">pull (RTSP/HTTP/HLS/SRT)</option>
        <option value="publisher">publisher (ffmpeg/OBS đẩy vào)</option>
      </select></label>
      <label>Transport<select name="transport"><option value="tcp">tcp</option><option value="udp">udp</option></select></label>
      <label class="full" id="urlWrap">URL<input name="url" placeholder="rtsp://user:pass@192.168.1.50:554/Streaming/Channels/101" /></label>
      <div class="full hint" id="pubHint" style="display:none">Source publisher không cần URL. Sau khi tạo, đẩy stream vào path hiển thị trong bảng.</div>
    </form>`,
    footer: `<button data-close>Huỷ</button><button class="primary" data-act="source-save">Tạo</button>`,
  });
  const kind = m.q('[name=kind]');
  const sync = () => {
    const pub = kind.value === 'publisher';
    m.q('#urlWrap').style.display = pub ? 'none' : '';
    m.q('#pubHint').style.display = pub ? '' : 'none';
  };
  kind.addEventListener('change', sync);
  sync();
  m.q('[data-act=source-save]').addEventListener('click', async () => {
    const f = new FormData(m.q('#srcForm'));
    try {
      await api('/sources', {
        method: 'POST',
        body: {
          name: f.get('name'),
          kind: f.get('kind'),
          transport: f.get('transport'),
          url: f.get('url') || '',
        },
      });
      m.close();
      toast('Đã tạo source', 'ok');
      await pageSources();
      await loadOverview();
    } catch (err) { toast(err.message, 'err'); }
  });
}

/* ------------------------------------------------------------------ cameras */

async function pageCameras() {
  if (!state.sources.length) state.sources = (await api('/sources')).items;
  const c = state.cam;
  const q = new URLSearchParams({ page: c.page, pageSize: c.pageSize });
  if (c.q) q.set('q', c.q);
  if (c.st) q.set('state', c.st);
  if (c.group) q.set('group', c.group);
  const out = await api('/cameras?' + q.toString());
  c.items = out.items;
  c.total = out.total;
  const pages = Math.max(1, Math.ceil(c.total / c.pageSize));
  const selCount = c.sel.size;
  const srcName = (id) => (state.sources.find((s) => s.id === id) || {}).name || '—';

  $('#view').innerHTML = `
    <div class="row">
      <input id="camQ" placeholder="Tìm theo tên / slug / group" value="${esc(c.q)}" style="max-width:260px" />
      <select id="camState" style="max-width:150px">
        <option value="">Mọi trạng thái</option>
        ${['ready', 'idle', 'offline', 'unknown', 'disabled', 'error'].map((s) => `<option value="${s}" ${c.st === s ? 'selected' : ''}>${s}</option>`).join('')}
      </select>
      <input id="camGroup" placeholder="Group" value="${esc(c.group)}" style="max-width:140px" />
      <button data-act="cam-search">Lọc</button>
      <span class="spacer"></span>
      <a class="badge" href="/api/v1/cameras/export?token=${encodeURIComponent(state.token)}">⬇ CSV</a>
      <button class="primary" data-act="cam-new">+ Tạo camera ảo</button>
    </div>

    ${selCount ? `<div class="panel"><div class="panel-body row">
      <b>${selCount} camera được chọn</b><span class="spacer"></span>
      <button class="sm" data-act="bulk-enable">Bật</button>
      <button class="sm" data-act="bulk-disable">Tắt</button>
      <button class="sm" data-act="bulk-rotate">Đổi mật khẩu</button>
      <button class="sm danger" data-act="bulk-delete">Xoá</button>
      <button class="sm ghost" data-act="bulk-clear">Bỏ chọn</button>
    </div></div>` : ''}

    <div class="panel">
      <div class="tablewrap"><table>
        <thead><tr>
          <th style="width:28px"><input type="checkbox" id="selAll" ${c.items.length && c.items.every((x) => c.sel.has(x.id)) ? 'checked' : ''} /></th>
          <th>Tên / slug</th><th>Source</th><th>Group</th><th>State</th><th class="num">Readers</th><th class="num">In</th><th class="num">Out</th><th></th>
        </tr></thead>
        <tbody>
        ${c.items.map((x) => `<tr>
          <td><input type="checkbox" data-sel="${x.id}" ${c.sel.has(x.id) ? 'checked' : ''} /></td>
          <td><b>${esc(x.name)}</b><div class="slug">${esc(x.slug)}</div></td>
          <td>${esc(x.sourceName || srcName(x.sourceId))}</td>
          <td>${esc(x.group || '—')}</td>
          <td><span class="state ${esc(x.runtime.state)}">${esc(x.runtime.state)}</span></td>
          <td class="num">${x.runtime.readers}</td>
          <td class="num">${kbps(x.runtime.inKbps)}</td>
          <td class="num">${kbps(x.runtime.outKbps)}</td>
          <td class="row">
            <button class="sm" data-act="cam-open" data-id="${x.id}">Chi tiết</button>
            <button class="sm" data-act="cam-copy" data-id="${x.id}" title="Copy RTSP">⧉</button>
            <button class="sm danger" data-act="cam-del" data-id="${x.id}" data-slug="${esc(x.slug)}">✕</button>
          </td>
        </tr>`).join('') || '<tr><td colspan="9" class="muted">Chưa có camera ảo. Bấm “Tạo camera ảo”.</td></tr>'}
        </tbody>
      </table></div>
      <div class="panel-body row muted small">
        <span>${fmt(c.total)} camera · trang ${c.page}/${pages}</span><span class="spacer"></span>
        <button class="sm" data-act="page-prev" ${c.page <= 1 ? 'disabled' : ''}>← Trước</button>
        <button class="sm" data-act="page-next" ${c.page >= pages ? 'disabled' : ''}>Sau →</button>
      </div>
    </div>`;

  $('#camState').addEventListener('change', (e) => { c.st = e.target.value; c.page = 1; c.sel.clear(); pageCameras(); });
  $('#camQ').addEventListener('keydown', (e) => { if (e.key === 'Enter') { c.q = e.target.value; c.page = 1; c.sel.clear(); pageCameras(); } });
  $('#camGroup').addEventListener('keydown', (e) => { if (e.key === 'Enter') { c.group = e.target.value; c.page = 1; c.sel.clear(); pageCameras(); } });
  $('#selAll').addEventListener('change', (e) => {
    c.items.forEach((x) => (e.target.checked ? c.sel.add(x.id) : c.sel.delete(x.id)));
    pageCameras();
  });
  document.querySelectorAll('[data-sel]').forEach((el) => el.addEventListener('change', (e) => {
    const id = e.target.dataset.sel;
    e.target.checked ? c.sel.add(id) : c.sel.delete(id);
    pageCameras();
  }));
}

async function cameraCreateModal() {
  if (!state.sources.length) state.sources = (await api('/sources')).items;
  if (!state.sources.length) {
    toast('Cần tạo source trước', 'err');
    return;
  }
  const srcOpts = state.sources.map((s) => `<option value="${s.id}">${esc(s.name)} (${s.kind})</option>`).join('');
  const m = modal({
    title: 'Tạo camera ảo',
    body: `<form id="camForm" class="formgrid">
      <label class="full">Source<select name="sourceId" required>${srcOpts}</select></label>
      <label>Prefix tên<input name="prefix" value="cam" required /></label>
      <label>Số lượng<input name="count" type="number" min="1" max="500" value="50" /></label>
      <label>Bắt đầu từ số<input name="startIndex" type="number" min="1" value="1" /></label>
      <label>Group<input name="group" placeholder="lab-test" /></label>
      <label>Mode<select name="mode"><option value="proxy">proxy (mỗi camera 1 session)</option><option value="redirect">redirect (tiết kiệm băng thông)</option></select></label>
      <label>Max readers / camera<input name="maxReaders" type="number" min="0" value="4" /></label>
      <label class="full">Mật khẩu cố định (bỏ trống = random mỗi camera)<input name="readPass" placeholder="để trống" /></label>
      <label class="full row" style="grid-template-columns:auto 1fr; display:flex; align-items:center; gap:8px">
        <input type="checkbox" name="onDemand" checked style="width:auto" /> <span>Tạo on-demand (chỉ chiếm tài nguyên khi có client)</span>
      </label>
      <div class="full hint">Slug sẽ là <code>prefix-0001</code> … (viết thường, không dấu). Mật khẩu hiển thị 1 lần sau khi tạo, có thể tải CSV ở danh sách camera.</div>
    </form>`,
    footer: `<button data-close>Huỷ</button><button class="primary" data-act="cam-save">Tạo</button>`,
  });
  m.q('[data-act=cam-save]').addEventListener('click', async () => {
    const f = new FormData(m.q('#camForm'));
    try {
      const out = await api('/cameras/bulk', {
        method: 'POST',
        body: {
          sourceId: f.get('sourceId'),
          count: Number(f.get('count')),
          prefix: f.get('prefix'),
          startIndex: Number(f.get('startIndex')),
          group: f.get('group'),
          mode: f.get('mode'),
          maxReaders: Number(f.get('maxReaders')),
          readPass: f.get('readPass') || '',
          onDemand: f.get('onDemand') === 'on',
        },
      });
      m.close();
      toast(`Đã tạo ${out.created} camera`, 'ok');
      showCreated(out.items);
      state.cam.page = 1;
      await pageCameras();
      await loadOverview();
    } catch (err) { toast(err.message, 'err'); }
  });
}

function showCreated(items) {
  const csv = ['name,slug,user,password', ...items.map((i) => [i.name, i.slug, i.readUser, i.readPass].join(','))].join('\n');
  const m = modal({
    title: 'Thông tin đăng nhập đã tạo',
    body: `<div class="hint">Lưu lại ngay — mật khẩu chỉ hiển thị ở đây (có thể tải lại ở “CSV”).</div>
      <pre id="createdCsv" style="max-height:320px">${esc(csv)}</pre>`,
    footer: `<button data-close>Đóng</button><button class="primary" data-copy-text="${esc(csv)}">Copy CSV</button>`,
  });
  m.q('[data-copy-text]').addEventListener('click', () => copy(csv));
}

async function cameraDetail(id) {
  const out = await api('/cameras/' + id);
  const c = out.camera;
  const p = out.preview;
  const m = modal({
    title: c.name + ' · ' + c.slug,
    body: `
      <div class="row">
        <span class="state ${esc(c.runtime.state)}">${esc(c.runtime.state)}</span>
        <span class="muted small">readers ${c.runtime.readers} · in ${kbps(c.runtime.inKbps)} · out ${kbps(c.runtime.outKbps)}</span>
        <span class="spacer"></span>
        <span class="muted small">${esc(c.sourceName || '')}</span>
      </div>
      <div class="panel">
        <div class="panel-body" style="display:grid; gap:10px">
          ${urlRow('RTSP', p.rtsp)}
          ${urlRow('HLS', p.hls)}
          ${urlRow('WebRTC (WHEP)', p.whep)}
        </div>
      </div>
      <div class="row">
        <button class="sm" data-act="cam-preview" data-url="${esc(p.whep)}">▶ Xem preview (WHEP)</button>
        <button class="sm" data-act="cam-enable" data-id="${c.id}" data-on="${c.enabled ? '0' : '1'}">${c.enabled ? 'Tắt camera' : 'Bật camera'}</button>
        <button class="sm" data-act="cam-restart" data-id="${c.id}">Restart path</button>
        <button class="sm" data-act="cam-rotate" data-id="${c.id}">Đổi mật khẩu</button>
        <span class="spacer"></span>
        <button class="sm danger" data-act="cam-del" data-id="${c.id}" data-slug="${esc(c.slug)}">Xoá</button>
      </div>
      <div id="previewWrap"></div>
      <form id="camEditForm" class="formgrid">
        <label>Tên<input name="name" value="${esc(c.name)}" /></label>
        <label>Group<input name="group" value="${esc(c.group)}" /></label>
        <label>Max readers<input name="maxReaders" type="number" min="0" value="${c.maxReaders}" /></label>
        <label>Mode<select name="mode">
          <option value="proxy" ${c.mode === 'proxy' ? 'selected' : ''}>proxy</option>
          <option value="redirect" ${c.mode === 'redirect' ? 'selected' : ''}>redirect</option>
        </select></label>
        <label>IP allowlist (CIDR, phẩy)<input name="ipAllowlist" value="${esc(c.ipAllowlist || '')}" placeholder="10.0.0.0/8" /></label>
        <label>Đổi mật khẩu<input name="readPass" placeholder="để trống = giữ nguyên" /></label>
        <label class="full row" style="display:flex;align-items:center;gap:8px">
          <input type="checkbox" name="onDemand" ${c.onDemand ? 'checked' : ''} style="width:auto" /> <span>On-demand source</span>
        </label>
        <div class="full row">
          <button class="primary sm" data-act="cam-save-edit" data-id="${c.id}">Lưu thay đổi</button>
          <span class="muted small">Credentials: ${esc(c.readUser)} / ${c.hasPass ? '••••••' : '(none)'}</span>
        </div>
      </form>
      <details><summary>Metadata giả lập</summary>
        <div class="panel-body muted small">${esc(c.vendor || '—')} · ${esc(c.model || '—')} · SN ${esc(c.serial || '—')} · FW ${esc(c.firmware || '—')}</div>
      </details>`,
    footer: `<button data-close>Đóng</button>`,
  });
  m.q('[data-act=cam-preview]')?.addEventListener('click', () => {
    m.q('#previewWrap').innerHTML = `<div class="panel"><div class="panel-body">
      <div class="hint">WHEP (WebRTC) — độ trễ thấp. Nếu không hiện, kiểm tra cổng 8889/8189.</div>
      <iframe src="${esc(p.whep)}?controls=false" style="width:100%;height:360px;border:0;border-radius:10px;background:#000"></iframe>
    </div></div>`;
  });
  m.q('[data-act=cam-save-edit]')?.addEventListener('click', async () => {
    const f = new FormData(m.q('#camEditForm'));
    try {
      await api('/cameras/' + c.id, {
        method: 'PATCH',
        body: {
          name: f.get('name'),
          group: f.get('group'),
          maxReaders: Number(f.get('maxReaders')),
          mode: f.get('mode'),
          ipAllowlist: f.get('ipAllowlist'),
          readPass: f.get('readPass') || '',
          onDemand: f.get('onDemand') === 'on',
        },
      });
      toast('Đã lưu', 'ok');
      m.close();
      await pageCameras();
    } catch (err) { toast(err.message, 'err'); }
  });
  m.q('[data-act=cam-enable]')?.addEventListener('click', async (e) => {
    const on = e.currentTarget.dataset.on === '1';
    await bulkAction([c.id], on ? 'enable' : 'disable');
    m.close();
  });
  m.q('[data-act=cam-restart]')?.addEventListener('click', async () => {
    try { await api(`/cameras/${c.id}/actions/restart`, { method: 'POST' }); toast('Đã restart path', 'ok'); } catch (err) { toast(err.message, 'err'); }
  });
  m.q('[data-act=cam-rotate]')?.addEventListener('click', async () => {
    try {
      const out = await api(`/cameras/${c.id}/actions/rotate-password`, { method: 'POST' });
      toast('Mật khẩu mới: ' + out.readPass, 'ok');
    } catch (err) { toast(err.message, 'err'); }
  });
  m.q('[data-act=cam-del]')?.addEventListener('click', async () => {
    if (!confirm('Xoá camera ' + c.slug + '?')) return;
    await deleteCamera(c.id, c.slug);
    m.close();
  });
}

function urlRow(label, url) {
  return `<div class="urlbox"><span class="muted small" style="min-width:92px">${esc(label)}</span><code>${esc(url)}</code>
    <button class="sm" data-copy="${esc(url)}">Copy</button></div>`;
}

async function bulkAction(ids, action) {
  try {
    const out = await api('/cameras/bulk-action', { method: 'POST', body: { ids, action } });
    toast('OK: ' + (out.affected ?? out.items?.length ?? 0) + ' camera', 'ok');
    state.cam.sel.clear();
    await pageCameras();
    await loadOverview();
  } catch (err) { toast(err.message, 'err'); }
}

async function deleteCamera(id, slug) {
  try {
    await api('/cameras/' + id, { method: 'DELETE' });
    toast('Đã xoá ' + slug, 'ok');
    state.cam.sel.delete(id);
    await pageCameras();
    await loadOverview();
  } catch (err) { toast(err.message, 'err'); }
}

/* ------------------------------------------------------------------ monitor */

async function pageMonitor() {
  const out = await api('/cameras?pageSize=200&page=1');
  const sorted = out.items.slice().sort((a, b) => b.runtime.readers - a.runtime.readers || a.slug.localeCompare(b.slug));
  const o = state.overview || {};
  $('#view').innerHTML = `
    <div class="cards">
      <div class="card"><h3>Readers</h3><div class="value" id="mon-readers">${fmt(state.stats?.readers || 0)}</div></div>
      <div class="card"><h3>Egress</h3><div class="value" id="mon-out">${kbps(state.stats?.outKbps || 0)}</div></div>
      <div class="card"><h3>Ready</h3><div class="value">${fmt(o.camerasReady || 0)}</div><div class="sub">tổng ${fmt(o.camerasTotal || 0)}</div></div>
      <div class="card"><h3>Managed paths</h3><div class="value">${fmt(state.stats?.managedPaths || 0)}</div><div class="sub">adds ${state.stats?.adds || 0} · patches ${state.stats?.patches || 0} · deletes ${state.stats?.deletes || 0}</div></div>
    </div>
    <div class="panel">
      <div class="panel-head"><h2>Camera theo tải</h2><span class="spacer"></span><span class="muted small">tự cập nhật mỗi 2s</span></div>
      <div class="tablewrap"><table><thead><tr><th>Slug</th><th>State</th><th class="num">Readers</th><th class="num">In</th><th class="num">Out</th><th>Lỗi</th></tr></thead><tbody>
      ${sorted.slice(0, 60).map((c) => `<tr><td class="slug">${esc(c.slug)}</td>
        <td><span class="state ${esc(c.runtime.state)}">${esc(c.runtime.state)}</span></td>
        <td class="num">${c.runtime.readers}</td><td class="num">${kbps(c.runtime.inKbps)}</td><td class="num">${kbps(c.runtime.outKbps)}</td>
        <td class="muted small">${esc(c.runtime.lastError || '')}</td></tr>`).join('')}
      </tbody></table></div>
    </div>`;
  // refresh every 5s while on this page
  state.timer = setInterval(() => {
    if (state.page !== 'monitor') { clearInterval(state.timer); state.timer = null; return; }
    pageMonitor().catch(() => {});
  }, 5000);
}

/* ------------------------------------------------------------------ loadtest */

async function pageLoadtest() {
  $('#view').innerHTML = `
    <div class="panel">
      <div class="panel-head"><h2>Load test — mô phỏng N client RTSP</h2></div>
      <div class="panel-body">
        <form id="ltForm" class="formgrid">
          <label>Số client<input name="count" type="number" value="100" min="1" max="2000" /></label>
          <label>Concurrency<input name="concurrency" type="number" value="20" min="1" max="200" /></label>
          <label>Timeout (ms)<input name="timeoutMs" type="number" value="8000" min="500" /></label>
          <label class="row" style="align-items:flex-end"><button class="primary" data-act="lt-run">▶ Chạy</button></label>
        </form>
        <div class="hint">Mỗi client thực hiện RTSP DESCRIBE (qua auth thật) rồi đóng — đo khả năng đáp ứng hàng trăm kết nối. Client xoay vòng trên các camera đang bật.</div>
      </div>
    </div>
    <div id="ltResult"></div>`;
  $('#ltForm').querySelector('[data-act=lt-run]').addEventListener('click', async (e) => {
    e.preventDefault();
    const f = new FormData($('#ltForm'));
    try {
      const run = await api('/loadtest', {
        method: 'POST',
        body: { count: Number(f.get('count')), concurrency: Number(f.get('concurrency')), timeoutMs: Number(f.get('timeoutMs')) },
      });
      renderRun({ ...run, status: 'running' });
      pollRun(run.id);
    } catch (err) { toast(err.message, 'err'); }
  });
}

function renderRun(r) {
  const done = r.status === 'done';
  const total = (r.ok || 0) + (r.fail || 0);
  const pct = total ? Math.round((r.ok / total) * 100) : 0;
  const errs = Object.entries(r.errors || {}).filter(([k]) => k && k !== 'ok').sort((a, b) => b[1] - a[1]).slice(0, 8);
  $('#ltResult').innerHTML = `
    <div class="panel">
      <div class="panel-head"><h2>Kết quả ${done ? '' : '<span class="spinner"></span>'}</h2><span class="spacer"></span><span class="muted small">${esc(r.id)}</span></div>
      <div class="panel-body" style="display:grid;gap:14px">
        <div class="cards">
          <div class="card"><h3>Thành công</h3><div class="value">${pct}%</div><div class="sub">${r.ok || 0}/${total} · fail ${r.fail || 0}</div></div>
          <div class="card"><h3>Latency p50 / p95</h3><div class="value">${r.p50Ms || 0}<span class="muted" style="font-size:14px"> / ${r.p95Ms || 0} ms</span></div><div class="sub">max ${r.maxMs || 0} ms · avg ${r.avgMs || 0} ms</div></div>
          <div class="card"><h3>Thời gian</h3><div class="value">${r.durationMs || 0}<span class="muted" style="font-size:14px"> ms</span></div><div class="sub">${r.count} client · conc ${r.concurrency}</div></div>
        </div>
        ${errs.length ? `<div><h3 class="muted small">Lỗi</h3><pre>${esc(errs.map(([k, v]) => v + '× ' + k).join('\n'))}</pre></div>` : ''}
      </div>
    </div>`;
}

async function pollRun(id) {
  const tick = async () => {
    try {
      const run = await api('/loadtest/' + id);
      renderRun(run);
      if (run.status !== 'done') setTimeout(tick, 700);
    } catch (err) { toast(err.message, 'err'); }
  };
  setTimeout(tick, 500);
}

/* ------------------------------------------------------------------ events/system */

async function pageEvents() {
  const out = await api('/events?limit=200');
  $('#view').innerHTML = `<div class="panel"><div class="panel-head"><h2>Sự kiện</h2><span class="spacer"></span><button class="sm" data-act="refresh">↻</button></div>
    <div class="panel-body">${eventsTable(out.items)}</div></div>`;
}

async function pageSystem() {
  const sys = await api('/system');
  setMediaBadge(sys.mediaOnline);
  $('#view').innerHTML = `
    <div class="cards">
      <div class="card"><h3>Control plane</h3><div class="value">v${esc(sys.version)}</div><div class="sub">uptime ${Math.floor((sys.uptimeSec || 0) / 60)}m</div></div>
      <div class="card"><h3>MediaMTX</h3><div class="value">${sys.mediaOnline ? 'online' : 'offline'}</div><div class="sub">${esc(sys.media?.version || sys.media?.error || '')}</div></div>
      <div class="card"><h3>Managed paths</h3><div class="value">${fmt(sys.stats?.managedPaths || 0)}</div><div class="sub">${esc(sys.mediaRtsp)}</div></div>
    </div>
    <div class="panel"><div class="panel-head"><h2>Cấu hình công bố ra client</h2></div><div class="panel-body">
      <div class="urlbox"><span class="muted small" style="min-width:110px">RTSP</span><code>${esc(sys.publicRtsp)}</code></div>
      <div class="urlbox" style="margin-top:8px"><span class="muted small" style="min-width:110px">HLS</span><code>${esc(sys.publicHls)}</code></div>
      <div class="urlbox" style="margin-top:8px"><span class="muted small" style="min-width:110px">WebRTC</span><code>${esc(sys.publicWebrtc)}</code></div>
      <div class="hint" style="margin-top:10px">Prefix path: cameras <code>${esc(sys.prefixes?.cameras)}</code>, sources <code>${esc(sys.prefixes?.sources)}</code>. Đổi bằng biến môi trường <code>RC_PUBLIC_RTSP</code>, <code>RC_PUBLIC_HLS</code>, <code>RC_PUBLIC_WEBRTC</code>.</div>
    </div></div>
    <div class="panel"><div class="panel-head"><h2>Raw</h2></div><div class="panel-body"><pre>${esc(JSON.stringify(sys, null, 2))}</pre></div></div>`;
}

/* ------------------------------------------------------------------ modal */

function modal({ title, body, footer }) {
  const root = $('#modalRoot');
  root.innerHTML = `<div class="modal-backdrop"><div class="modal">
    <div class="modal-head"><h2>${esc(title)}</h2><span class="spacer"></span><button class="ghost sm" data-close>✕</button></div>
    <div class="modal-body">${body}</div>
    ${footer ? `<div class="modal-foot">${footer}</div>` : ''}
  </div></div>`;
  const close = () => { root.innerHTML = ''; };
  root.querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', close));
  root.querySelector('.modal-backdrop').addEventListener('mousedown', (e) => {
    if (e.target.classList.contains('modal-backdrop')) close();
  });
  return { root, close, q: (s) => root.querySelector(s) };
}

/* ------------------------------------------------------------------ events */

document.addEventListener('click', async (e) => {
  const btn = e.target.closest('[data-act]');
  const copyBtn = e.target.closest('[data-copy]');
  if (copyBtn) { copy(copyBtn.dataset.copy); return; }
  if (!btn) return;
  const { act } = btn.dataset;
  const id = btn.dataset.id;

  try {
    switch (act) {
      case 'refresh': await navigate(state.page); break;
      case 'source-new': sourceModal(); break;
      case 'source-save': break; // handled locally
      case 'source-test': {
        toast('Đang test…');
        const res = await api(`/sources/${id}/test`, { method: 'POST' });
        toast(res.ok ? `OK · ${res.latencyMs} ms · ${res.server || ''}` : `Lỗi: ${res.error || res.status}`, res.ok ? 'ok' : 'err');
        await pageSources();
        break;
      }
      case 'source-toggle': {
        await api('/sources/' + id, { method: 'PATCH', body: { enabled: btn.dataset.enabled !== '1' } });
        await pageSources();
        break;
      }
      case 'source-del': {
        if (!confirm('Xoá source "' + btn.dataset.name + '" và toàn bộ camera ảo dùng nó?')) return;
        await api('/sources/' + id, { method: 'DELETE' });
        toast('Đã xoá source', 'ok');
        await pageSources();
        await loadOverview();
        break;
      }
      case 'cam-new': await cameraCreateModal(); break;
      case 'cam-open': await cameraDetail(id); break;
      case 'cam-copy': {
        const out = await api('/cameras/' + id + '/preview');
        copy(out.rtsp);
        break;
      }
      case 'cam-del': {
        if (!confirm('Xoá camera ' + btn.dataset.slug + '?')) return;
        await deleteCamera(id, btn.dataset.slug);
        break;
      }
      case 'cam-enable': await bulkAction([id], btn.dataset.on === '1' ? 'enable' : 'disable'); break;
      case 'bulk-enable': await bulkAction([...state.cam.sel], 'enable'); break;
      case 'bulk-disable': await bulkAction([...state.cam.sel], 'disable'); break;
      case 'bulk-delete': {
        if (!confirm(`Xoá ${state.cam.sel.size} camera?`)) return;
        await bulkAction([...state.cam.sel], 'delete');
        break;
      }
      case 'bulk-rotate': {
        const out = await api('/cameras/bulk-action', { method: 'POST', body: { ids: [...state.cam.sel], action: 'rotate-password' } });
        const csv = ['slug,user,password'].concat((out.items || []).map((i) => i.slug + ',,' + i.readPass)).join('\n');
        showCreated((out.items || []).map((i) => ({ name: i.slug, slug: i.slug, readUser: '', readPass: i.readPass })));
        await pageCameras();
        break;
      }
      case 'bulk-clear': state.cam.sel.clear(); await pageCameras(); break;
      case 'cam-search': {
        state.cam.q = $('#camQ')?.value || '';
        state.cam.group = $('#camGroup')?.value || '';
        state.cam.page = 1;
        await pageCameras();
        break;
      }
      case 'page-prev': state.cam.page = Math.max(1, state.cam.page - 1); await pageCameras(); break;
      case 'page-next': state.cam.page += 1; await pageCameras(); break;
      default: break;
    }
  } catch (err) {
    toast(err.message, 'err');
  }
});

$('#loginForm').addEventListener('submit', onLogin);
$('#logout').addEventListener('click', logout);

/* boot */
(async function boot() {
  if (!state.token) { showLogin(); return; }
  try {
    await start();
  } catch (err) {
    showLogin();
  }
})();
