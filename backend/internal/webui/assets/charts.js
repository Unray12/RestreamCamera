/* RestreamCamera — tiny canvas time-series chart (no dependencies).
 *
 *   const c = new TimeChart(el, { height: 240, fmt: fmtRate, windowSec: 300,
 *                                 series: [{ key: 'out', label: 'Egress', color: '#38bdf8' }] });
 *   c.setData(ts, { out: [...] });      // replace everything (ts in unix seconds)
 *   c.push(ts, { out: 1234 });          // append one live point
 *
 * Features: crisp HiDPI canvas, nice axis ticks, crosshair + tooltip, legend toggles,
 * stacked areas, shaded bands (load-test windows), gaps for missing values.
 */
'use strict';

const PALETTE = ['#38bdf8', '#f59e0b', '#a78bfa', '#34d399', '#f472b6', '#fb7185', '#facc15', '#2dd4bf', '#818cf8', '#fb923c', '#a3e635', '#94a3b8'];

const fmtRate = (k) => {
  k = Number(k) || 0;
  if (k >= 1e6) return (k / 1e6).toFixed(2) + ' Gbps';
  if (k >= 1000) return (k / 1000).toFixed(k >= 10000 ? 1 : 2) + ' Mbps';
  if (k >= 100) return Math.round(k) + ' kbps';
  return k.toFixed(k >= 10 ? 1 : 2).replace(/\.?0+$/, '') + ' kbps';
};
const fmtBytes = (b) => {
  b = Number(b) || 0;
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (i === 0 ? Math.round(b) : b.toFixed(b >= 100 ? 0 : 1)) + ' ' + u[i];
};
const fmtMs = (v) => {
  v = Number(v) || 0;
  if (v >= 1000) return (v / 1000).toFixed(2) + ' s';
  return (v >= 100 ? Math.round(v) : v.toFixed(1).replace(/\.0$/, '')) + ' ms';
};
const fmtPct = (v) => (Number(v) || 0).toFixed(v >= 10 ? 1 : 2).replace(/\.?0+$/, '') + ' %';
const fmtInt = (v) => String(Math.round(Number(v) || 0));
const pad2 = (n) => (n < 10 ? '0' : '') + n;
const clock = (ts, withSec) => {
  const d = new Date(ts * 1000);
  return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + (withSec ? ':' + pad2(d.getSeconds()) : '');
};

function niceStep(max, ticks) {
  const raw = max / Math.max(1, ticks);
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const n = raw / mag;
  const f = n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10;
  return f * mag;
}

class TimeChart {
  constructor(host, opts = {}) {
    this.o = Object.assign({ height: 220, fmt: fmtRate, stacked: false, fill: null, min: 0, windowSec: 300, emptyMax: 1, legend: true, yTicks: 4, step: 'line' }, opts);
    this.host = host;
    this.ts = [];
    this.bands = [];
    this.hover = -1;
    this.setSeries(this.o.series || []);
    host.classList.add('tc');
    host.innerHTML = '<div class="tc-legend"></div><div class="tc-wrap"><canvas></canvas><div class="tc-tip hidden"></div></div>';
    this.legendEl = host.querySelector('.tc-legend');
    this.wrap = host.querySelector('.tc-wrap');
    this.cv = host.querySelector('canvas');
    this.tip = host.querySelector('.tc-tip');
    this.wrap.style.height = this.o.height + 'px';
    if (!this.o.legend) this.legendEl.style.display = 'none';
    this.cv.addEventListener('mousemove', (e) => this._onMove(e));
    this.cv.addEventListener('mouseleave', () => { this.hover = -1; this.tip.classList.add('hidden'); this._legendValues(); this._schedule(); });
    this.cv.addEventListener('touchstart', (e) => this._onMove(e.touches[0]), { passive: true });
    this.cv.addEventListener('touchmove', (e) => this._onMove(e.touches[0]), { passive: true });
    this.ro = new ResizeObserver(() => this._schedule());
    this.ro.observe(this.wrap);
    this.legendEl.addEventListener('click', (e) => {
      const it = e.target.closest('[data-k]');
      if (!it) return;
      const s = this.series.find((x) => x.key === it.dataset.k);
      if (s) { s.vis = !s.vis; this._legend(); this._schedule(); }
    });
    this._legend();
    this._schedule();
  }

  destroy() { this.ro.disconnect(); this.dead = true; }

  setSeries(defs) {
    const old = new Map((this.series || []).map((s) => [s.key, s]));
    this.series = defs.map((d, i) => {
      const prev = old.get(d.key);
      return { color: PALETTE[i % PALETTE.length], dash: false, ...d, vis: prev ? prev.vis : d.vis !== false, data: prev ? prev.data : [] };
    });
    if (this.legendEl) { this._legend(); this._schedule(); }
  }

  setData(ts, map) {
    this.ts = ts.slice();
    for (const s of this.series) s.data = (map[s.key] || []).slice();
    for (const s of this.series) while (s.data.length < this.ts.length) s.data.unshift(null);
    this._trim();
    this._legendValues();
    this._schedule();
  }

  push(ts, vals) {
    if (this.ts.length && ts <= this.ts[this.ts.length - 1]) ts = this.ts[this.ts.length - 1] + 1;
    this.ts.push(ts);
    for (const s of this.series) s.data.push(vals[s.key] ?? null);
    this._trim();
    if (this.hover < 0) this._legendValues();
    this._schedule();
  }

  setBands(b) { this.bands = b || []; this._schedule(); }
  setWindow(sec) { this.o.windowSec = sec; this._schedule(); }

  _trim() {
    const cap = Math.max(60, Math.ceil(this.o.windowSec * 1.2) + 10);
    const extra = this.ts.length - cap;
    if (extra > 0) {
      this.ts.splice(0, extra);
      for (const s of this.series) s.data.splice(0, extra);
    }
  }

  _legend() {
    this.legendEl.innerHTML = this.series.map((s) =>
      `<span class="tc-item ${s.vis ? '' : 'off'}" data-k="${esc(s.key)}" title="Bấm để ẩn/hiện"><i style="background:${s.color}"></i>${esc(s.label || s.key)}<b data-v="${esc(s.key)}"></b></span>`).join('');
    this._legendValues();
  }

  _legendValues(idx) {
    if (!this.legendEl) return;
    const i = idx != null && idx >= 0 ? idx : this.ts.length - 1;
    for (const s of this.series) {
      const el = this.legendEl.querySelector(`[data-v="${CSS.escape(s.key)}"]`);
      if (el) el.textContent = i >= 0 && s.data[i] != null ? this.o.fmt(s.data[i]) : '';
    }
  }

  _schedule() {
    if (this._raf || this.dead) return;
    this._raf = requestAnimationFrame(() => { this._raf = 0; if (!this.dead) this._draw(); });
  }

  _geom() {
    const W = this.wrap.clientWidth || 300;
    const H = this.o.height;
    return { W, H, l: this._l || 58, r: 10, t: 8, b: 22 };
  }

  _domain() {
    const xmax = this.ts.length ? this.ts[this.ts.length - 1] : Math.floor(Date.now() / 1000);
    return [xmax - this.o.windowSec, xmax];
  }

  _onMove(e) {
    if (!this.ts.length) return;
    const g = this._geom();
    const rect = this.cv.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const [x0, x1] = this._domain();
    const t = x0 + ((x - g.l) / (g.W - g.l - g.r)) * (x1 - x0);
    // nearest index by binary search
    let lo = 0, hi = this.ts.length - 1;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (this.ts[mid] < t) lo = mid + 1; else hi = mid; }
    if (lo > 0 && Math.abs(this.ts[lo - 1] - t) < Math.abs(this.ts[lo] - t)) lo--;
    this.hover = (x < g.l || x > g.W - g.r) ? -1 : lo;
    this._mouse = { x: e.clientX - rect.left, y: e.clientY - rect.top };
    this._legendValues(this.hover);
    this._schedule();
  }

  _draw() {
    const g = this._geom();
    const dpr = window.devicePixelRatio || 1;
    const cv = this.cv;
    if (cv.width !== Math.round(g.W * dpr) || cv.height !== Math.round(g.H * dpr)) {
      cv.width = Math.round(g.W * dpr);
      cv.height = Math.round(g.H * dpr);
      cv.style.width = g.W + 'px';
      cv.style.height = g.H + 'px';
    }
    const ctx = cv.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, g.W, g.H);
    const pw = g.W - g.l - g.r, ph = g.H - g.t - g.b;
    const [x0, x1] = this._domain();
    const X = (t) => g.l + ((t - x0) / (x1 - x0 || 1)) * pw;

    const vis = this.series.filter((s) => s.vis);
    // y range
    let ymax = 0;
    const stack = this.o.stacked;
    for (let i = 0; i < this.ts.length; i++) {
      if (this.ts[i] < x0) continue;
      let sum = 0;
      for (const s of vis) {
        const v = s.data[i];
        if (v == null) continue;
        if (stack) sum += v; else if (v > ymax) ymax = v;
      }
      if (stack && sum > ymax) ymax = sum;
    }
    const ymin = this.o.min;
    if (ymax <= ymin) ymax = ymin + this.o.emptyMax;
    const step = niceStep((ymax - ymin) * 1.04, this.o.yTicks);
    ymax = Math.ceil((ymax * 1.02) / step) * step;
    const Y = (v) => g.t + ph - ((v - ymin) / (ymax - ymin)) * ph;
    ctx.font = '11px ui-sans-serif, system-ui, sans-serif';
    let lw = 0;
    for (let v = ymin; v <= ymax + step * 0.001; v += step) lw = Math.max(lw, ctx.measureText(this.o.fmt(v)).width);
    const needL = Math.max(48, Math.ceil(lw) + 14);
    if (needL !== (this._l || 58)) { this._l = needL; this._schedule(); return; }

    // grid + y labels
    ctx.font = '11px ui-sans-serif, system-ui, sans-serif';
    ctx.textBaseline = 'middle';
    ctx.textAlign = 'right';
    ctx.lineWidth = 1;
    for (let v = ymin; v <= ymax + step * 0.001; v += step) {
      const y = Math.round(Y(v)) + 0.5;
      ctx.strokeStyle = 'rgba(148,163,184,.13)';
      ctx.beginPath(); ctx.moveTo(g.l, y); ctx.lineTo(g.W - g.r, y); ctx.stroke();
      ctx.fillStyle = '#8b9bb4';
      ctx.fillText(this.o.fmt(v), g.l - 7, y);
    }
    // x ticks
    const span = x1 - x0;
    const steps = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200];
    const want = Math.max(2, Math.floor(pw / 95));
    const xs = steps.find((s) => span / s <= want) || 43200;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'top';
    for (let t = Math.ceil(x0 / xs) * xs; t <= x1; t += xs) {
      const x = Math.round(X(t)) + 0.5;
      ctx.strokeStyle = 'rgba(148,163,184,.07)';
      ctx.beginPath(); ctx.moveTo(x, g.t); ctx.lineTo(x, g.t + ph); ctx.stroke();
      ctx.fillStyle = '#8b9bb4';
      ctx.fillText(clock(t, xs < 60), x, g.t + ph + 6);
    }

    // bands
    for (const b of this.bands) {
      const a = Math.max(X(b.start), g.l), z = Math.min(X(b.end), g.W - g.r);
      if (z <= a) continue;
      const wide = z - a > 90;
      ctx.fillStyle = 'rgba(245,158,11,.10)';
      ctx.fillRect(a, g.t, z - a, ph);
      ctx.fillStyle = 'rgba(245,158,11,.75)';
      ctx.textAlign = 'left';
      ctx.textBaseline = 'top';
      if (wide) ctx.fillText(b.label || 'load test', a + 4, g.t + 3);
    }

    // clip drawing to plot area
    ctx.save();
    ctx.beginPath(); ctx.rect(g.l, g.t - 2, pw, ph + 4); ctx.clip();
    const first = Math.max(0, this._lowerBound(x0) - 1);
    const fillAuto = this.o.fill == null ? vis.length <= 3 : this.o.fill;

    const base = new Array(this.ts.length).fill(0);
    for (const s of vis) {
      const tops = [];
      for (let i = first; i < this.ts.length; i++) {
        const v = s.data[i];
        tops.push(v == null ? null : (stack ? base[i] + v : v));
      }
      const path = (close) => {
        let open = false, lastX = 0;
        ctx.beginPath();
        tops.forEach((v, k) => {
          const i = first + k;
          if (v == null) { open = false; return; }
          const x = X(this.ts[i]), y = Y(v);
          if (!open) { ctx.moveTo(x, y); open = true; } else if (this.o.step === 'step') { ctx.lineTo(x, Y(tops[k - 1] ?? v)); ctx.lineTo(x, y); } else ctx.lineTo(x, y);
          lastX = x;
        });
        return lastX;
      };
      if (stack || fillAuto) {
        // fill down to the baseline (or the previous layer when stacked)
        ctx.beginPath();
        let started = false;
        tops.forEach((v, k) => {
          if (v == null) return;
          const x = X(this.ts[first + k]);
          if (!started) { ctx.moveTo(x, Y(v)); started = true; } else ctx.lineTo(x, Y(v));
        });
        if (started) {
          for (let k = tops.length - 1; k >= 0; k--) {
            if (tops[k] == null) continue;
            ctx.lineTo(X(this.ts[first + k]), Y(stack ? base[first + k] : ymin));
          }
          ctx.closePath();
          if (stack) ctx.fillStyle = s.color + '66';
          else {
            const gr = ctx.createLinearGradient(0, g.t, 0, g.t + ph);
            gr.addColorStop(0, s.color + '38');
            gr.addColorStop(1, s.color + '05');
            ctx.fillStyle = gr;
          }
          ctx.fill();
        }
      }
      path();
      ctx.strokeStyle = s.color;
      ctx.lineWidth = s.width || 1.6;
      ctx.setLineDash(s.dash ? [5, 4] : []);
      ctx.lineJoin = 'round';
      ctx.stroke();
      ctx.setLineDash([]);
      if (stack) for (let i = first; i < this.ts.length; i++) if (s.data[i] != null) base[i] += s.data[i];
    }
    ctx.restore();

    // empty state
    if (!this.ts.length) {
      ctx.fillStyle = '#64748b';
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      ctx.fillText('Đang chờ dữ liệu…', g.l + pw / 2, g.t + ph / 2);
    }

    // hover
    if (this.hover >= 0 && this.hover < this.ts.length) {
      const i = this.hover;
      const x = Math.round(X(this.ts[i])) + 0.5;
      ctx.strokeStyle = 'rgba(226,232,240,.35)';
      ctx.beginPath(); ctx.moveTo(x, g.t); ctx.lineTo(x, g.t + ph); ctx.stroke();
      let cum = 0;
      const rows = [];
      for (const s of vis) {
        const v = s.data[i];
        if (v == null) continue;
        cum += stack ? v : 0;
        const y = Y(stack ? cum : v);
        ctx.fillStyle = s.color;
        ctx.beginPath(); ctx.arc(X(this.ts[i]), y, 3.2, 0, Math.PI * 2); ctx.fill();
        rows.push({ s, v });
      }
      rows.sort((a, b) => b.v - a.v);
      const more = rows.length - 14;
      this.tip.innerHTML = `<div class="tc-tip-h">${clock(this.ts[i], true)}</div>` +
        rows.slice(0, 14).map((r) => `<div><i style="background:${r.s.color}"></i><span>${esc(r.s.label || r.s.key)}</span><b>${esc(this.o.fmt(r.v))}</b></div>`).join('') +
        (more > 0 ? `<div class="muted">+${more} nữa…</div>` : '');
      this.tip.classList.remove('hidden');
      const tw = this.tip.offsetWidth, th = this.tip.offsetHeight;
      let tx = (this._mouse?.x ?? x) + 14;
      if (tx + tw > g.W - 4) tx = (this._mouse?.x ?? x) - tw - 14;
      let ty = (this._mouse?.y ?? 20) - th / 2;
      ty = Math.max(2, Math.min(ty, g.H - th - 2));
      this.tip.style.transform = `translate(${Math.max(2, tx)}px, ${ty}px)`;
    } else {
      this.tip.classList.add('hidden');
    }
  }

  _lowerBound(t) {
    let lo = 0, hi = this.ts.length;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (this.ts[mid] < t) lo = mid + 1; else hi = mid; }
    return lo;
  }
}

/* inline SVG sparkline for table/cards */
function sparkSVG(vals, w = 110, h = 26, color = '#38bdf8') {
  if (!vals || vals.length < 2) return `<svg width="${w}" height="${h}"></svg>`;
  const max = Math.max(1e-9, ...vals);
  const dx = w / (vals.length - 1);
  const pts = vals.map((v, i) => `${(i * dx).toFixed(1)},${(h - 2 - (v / max) * (h - 4)).toFixed(1)}`).join(' ');
  return `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}"><polyline points="${pts}" fill="none" stroke="${color}" stroke-width="1.5" stroke-linejoin="round"/></svg>`;
}
