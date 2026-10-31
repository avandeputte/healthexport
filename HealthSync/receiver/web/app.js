/* Health Sync web app: a small single-page app over the receiver's /api. No build step; uPlot draws the charts and
   Leaflet the maps. Pages are hash routes (#/workouts, #/workout/<uuid>, #/metrics?types=…). */
'use strict';

// ---------- basics ----------

const $ = (sel, el = document) => el.querySelector(sel);
const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
const DAY = 86400;
// Axis steps for values in minutes, so ticks land on whole hours.
const MINUTE_INCRS = [15, 30, 60, 120, 180, 240, 360, 600, 1200, 1800, 3000, 6000, 12000];

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'html') el.innerHTML = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const svgNS = 'http://www.w3.org/2000/svg';
function s(tag, attrs, ...children) {
  const el = document.createElementNS(svgNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v);
  }
  for (const c of children.flat()) if (c != null) el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  return el;
}

function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

const settings = (() => {
  const defaults = { units: 'metric', maxHr: '', theme: 'auto' };
  try { return Object.assign(defaults, JSON.parse(localStorage.getItem('healthsync.settings') || '{}')); } catch { return defaults; }
})();
function saveSettings() { try { localStorage.setItem('healthsync.settings', JSON.stringify(settings)); } catch { /* private mode */ } }
function applyTheme() {
  if (settings.theme === 'auto') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme', settings.theme);
}
applyTheme();

const apiCache = new Map();
async function api(path, params = {}, { cache = true } = {}) {
  const q = new URLSearchParams({ ...params, tz: TZ });
  if (settings.maxHr) q.set('maxHr', settings.maxHr);
  const url = 'api/' + path + '?' + q;
  if (cache && apiCache.has(url)) {
    const hit = apiCache.get(url);
    if (Date.now() - hit.at < 60000) return hit.promise;
  }
  const promise = fetch(url, { credentials: 'same-origin' }).then(async r => {
    if (r.status === 401) { location.href = 'login.html'; throw new Error('not signed in'); }
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(body.error || r.statusText);
    return body;
  });
  apiCache.set(url, { at: Date.now(), promise });
  promise.catch(() => apiCache.delete(url));
  return promise;
}

// ---------- formatting ----------

const nf = {};
function fmtNum(v, dec = 0) {
  if (v == null || !isFinite(v)) return '—';
  const k = dec;
  nf[k] ||= new Intl.NumberFormat(undefined, { minimumFractionDigits: dec, maximumFractionDigits: dec });
  return nf[k].format(v);
}
function fmtCompact(v) {
  if (v == null || !isFinite(v)) return '';
  const a = Math.abs(v);
  if (a >= 1e6) return fmtNum(v / 1e6, 1) + 'M';
  if (a >= 1e4) return fmtNum(v / 1e3, 0) + 'k';
  if (a >= 100) return fmtNum(v, 0);
  if (a >= 10) return fmtNum(v, 1).replace(/\.0$/, '');
  return fmtNum(v, 2).replace(/\.?0+$/, '');
}
function fmtDuration(sec, short = false) {
  if (sec == null || !isFinite(sec)) return '—';
  sec = Math.round(sec);
  const hh = Math.floor(sec / 3600), mm = Math.floor(sec % 3600 / 60), ss = sec % 60;
  if (short) return hh ? `${hh}h ${String(mm).padStart(2, '0')}m` : `${mm}m`;
  return hh ? `${hh}:${String(mm).padStart(2, '0')}:${String(ss).padStart(2, '0')}` : `${mm}:${String(ss).padStart(2, '0')}`;
}
function fmtHM(minutes) {
  if (minutes == null || !isFinite(minutes)) return '—';
  const m = Math.round(minutes);
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}
const dateFmt = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
const dateShortFmt = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric' });
const weekdayFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short', month: 'short', day: 'numeric' });
const dateTimeFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short', year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
const timeFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
const fmtDate = t => t == null ? '—' : dateFmt.format(new Date(t * 1000));
const fmtDateShort = t => dateShortFmt.format(new Date(t * 1000));
const fmtDateTime = t => dateTimeFmt.format(new Date(t * 1000));
const fmtTime = t => timeFmt.format(new Date(t * 1000));
function fmtAgo(t) {
  const d = Date.now() / 1000 - t;
  if (d < 90) return 'just now';
  if (d < 3600) return `${Math.round(d / 60)} min ago`;
  if (d < 86400) return `${Math.round(d / 3600)} h ago`;
  return `${Math.round(d / 86400)} days ago`;
}
function isoDate(d) { // local yyyy-mm-dd
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}
function daysAgo(n) { const d = new Date(); d.setDate(d.getDate() - n); return isoDate(d); }
const today = () => isoDate(new Date());

const imperial = () => settings.units === 'imperial';
function fmtDist(m, dec) {
  if (m == null) return '—';
  if (imperial()) return fmtNum(m / 1609.344, dec ?? 2) + ' mi';
  return m < 1000 && dec == null ? fmtNum(m, 0) + ' m' : fmtNum(m / 1000, dec ?? 2) + ' km';
}
function distParts(m) {
  return imperial() ? [fmtNum(m / 1609.344, 2), 'mi'] : [fmtNum(m / 1000, 2), 'km'];
}
function fmtPace(secPerKm) {
  if (!secPerKm || !isFinite(secPerKm)) return '—';
  const v = imperial() ? secPerKm * 1.609344 : secPerKm;
  return fmtDuration(v) + (imperial() ? ' /mi' : ' /km');
}
function fmtSpeed(mps) {
  if (mps == null) return '—';
  return imperial() ? fmtNum(mps * 2.236936, 1) + ' mph' : fmtNum(mps * 3.6, 1) + ' km/h';
}
const fmtElev = m => m == null ? '—' : imperial() ? fmtNum(m * 3.28084, 0) + ' ft' : fmtNum(m, 0) + ' m';
const fmtKcal = k => k == null ? '—' : fmtNum(k, 0) + ' kcal';

// ---------- health types ----------

const TYPES = {
  step_count: ['Steps', 0], distance_walking_running: ['Walking + running distance', 2], distance_cycling: ['Cycling distance', 1],
  distance_swimming: ['Swimming distance', 0], active_energy_burned: ['Active energy', 0], basal_energy_burned: ['Resting energy', 0],
  apple_exercise_time: ['Exercise minutes', 0], apple_stand_time: ['Stand minutes', 0], apple_move_time: ['Move minutes', 0],
  flights_climbed: ['Flights climbed', 0], heart_rate: ['Heart rate', 0], resting_heart_rate: ['Resting heart rate', 0],
  walking_heart_rate_average: ['Walking heart rate', 0], heart_rate_variability_sdnn: ['Heart rate variability', 0],
  heart_rate_recovery_one_minute: ['Cardio recovery', 0], vo2_max: ['VO₂ max', 1], body_mass: ['Weight', 1],
  body_mass_index: ['Body mass index', 1], body_fat_percentage: ['Body fat', 1], lean_body_mass: ['Lean body mass', 1],
  height: ['Height', 0], waist_circumference: ['Waist', 1], oxygen_saturation: ['Blood oxygen', 1], respiratory_rate: ['Respiratory rate', 1],
  body_temperature: ['Body temperature', 1], apple_sleeping_wrist_temperature: ['Wrist temperature', 2],
  blood_pressure_systolic: ['Blood pressure (systolic)', 0], blood_pressure_diastolic: ['Blood pressure (diastolic)', 0],
  blood_glucose: ['Blood glucose', 0], walking_speed: ['Walking speed', 2], walking_step_length: ['Step length', 0],
  walking_asymmetry_percentage: ['Walking asymmetry', 1], walking_double_support_percentage: ['Double support time', 1],
  apple_walking_steadiness: ['Walking steadiness', 0], six_minute_walk_test_distance: ['Six-minute walk', 0],
  stair_ascent_speed: ['Stair speed up', 2], stair_descent_speed: ['Stair speed down', 2], running_speed: ['Running speed', 2],
  running_power: ['Running power', 0], running_stride_length: ['Stride length', 2], running_vertical_oscillation: ['Vertical oscillation', 1],
  running_ground_contact_time: ['Ground contact time', 0], cycling_power: ['Cycling power', 0], cycling_cadence: ['Cycling cadence', 0],
  cycling_speed: ['Cycling speed', 1], cycling_functional_threshold_power: ['Cycling FTP', 0], physical_effort: ['Physical effort', 1],
  time_in_daylight: ['Time in daylight', 0], environmental_audio_exposure: ['Environmental sound', 0],
  headphone_audio_exposure: ['Headphone audio', 0], sleep_analysis: ['Sleep', 1], mindful_session: ['Mindful minutes', 0],
  apple_stand_hour: ['Stand hours', 0], dietary_energy_consumed: ['Dietary energy', 0], dietary_water: ['Water', 0],
  dietary_caffeine: ['Caffeine', 0], dietary_protein: ['Protein', 0], number_of_alcoholic_beverages: ['Alcoholic drinks', 0],
  uv_exposure: ['UV index', 0], swimming_stroke_count: ['Swimming strokes', 0], push_count: ['Wheelchair pushes', 0],
  underwater_depth: ['Underwater depth', 1], water_temperature: ['Water temperature', 1], workout_effort_score: ['Workout effort', 1],
  atrial_fibrillation_burden: ['AFib history', 1], electrodermal_activity: ['Electrodermal activity', 2],
};
const typeLabel = t => TYPES[t]?.[0] || t.replace(/_/g, ' ').replace(/^./, c => c.toUpperCase());
const typeDec = t => TYPES[t]?.[1] ?? 1;
const PRETTY_UNITS = { 'count/min': 'bpm', count: '', degC: '°C', degF: '°F', dBASPL: 'dB', 'mL/kg·min': 'mL/kg/min', 'mL/min·kg': 'mL/kg/min', 'ml/(kg*min)': 'mL/kg/min',
  'mL/(kg·min)': 'mL/kg/min', 'kcal/hr·kg': 'kcal/h/kg', 'kcal/(kg*hr)': 'kcal/h/kg', mmHg: 'mmHg', 'mg/dL': 'mg/dL', 'm/s': 'm/s' };
const prettyUnit = u => u == null ? '' : (u in PRETTY_UNITS ? PRETTY_UNITS[u] : u);
// HealthKit percentages are fractions; show them out of 100.
const scaleFor = unit => unit === '%' ? 100 : 1;

const COMMON_TYPES = ['step_count', 'active_energy_burned', 'apple_exercise_time', 'distance_walking_running', 'flights_climbed',
  'heart_rate', 'resting_heart_rate', 'heart_rate_variability_sdnn', 'walking_heart_rate_average', 'vo2_max', 'body_mass',
  'body_fat_percentage', 'oxygen_saturation', 'respiratory_rate', 'apple_sleeping_wrist_temperature', 'time_in_daylight',
  'walking_speed', 'running_power'];

const ACTIVITY_NAMES = { highIntensityIntervalTraining: 'HIIT', traditionalStrengthTraining: 'Strength training',
  functionalStrengthTraining: 'Functional strength', mixedCardio: 'Mixed cardio', crossTraining: 'Cross training',
  preparationAndRecovery: 'Recovery', mindAndBody: 'Mind & body', swimBikeRun: 'Multisport', stairClimbing: 'Stair climbing',
  coreTraining: 'Core training', cardioDance: 'Dance', socialDance: 'Social dance', wheelchairWalkPace: 'Wheelchair walk',
  wheelchairRunPace: 'Wheelchair run', other: 'Other' };
const activityName = a => ACTIVITY_NAMES[a] || (a || 'Workout').replace(/([a-z])([A-Z])/g, '$1 $2').replace(/^./, c => c.toUpperCase());
const ACTIVITY_COLORS = { running: '#e8445a', walking: '#16a34a', hiking: '#65a30d', cycling: '#2563eb', swimming: '#06b6d4',
  traditionalStrengthTraining: '#8b5cf6', functionalStrengthTraining: '#a855f7', highIntensityIntervalTraining: '#f97316',
  yoga: '#db2777', rowing: '#0ea5e9', elliptical: '#f59e0b', mixedCardio: '#ef4444', coreTraining: '#c026d3' };
const PALETTE = ['#e8445a', '#2563eb', '#16a34a', '#f59e0b', '#8b5cf6', '#06b6d4', '#db2777', '#65a30d', '#f97316', '#0f766e'];
function activityColor(a) {
  if (ACTIVITY_COLORS[a]) return ACTIVITY_COLORS[a];
  let x = 0;
  for (const c of a || '') x = (x * 31 + c.charCodeAt(0)) >>> 0;
  return PALETTE[x % PALETTE.length];
}
const paceSports = new Set(['running', 'walking', 'hiking', 'wheelchairRunPace', 'wheelchairWalkPace', 'trackAndField']);

// ---------- page lifecycle ----------

let cleanups = [];
function onCleanup(f) { cleanups.push(f); }
function resetPage() { for (const f of cleanups.splice(0)) { try { f(); } catch (e) { console.error(e); } } }

function loading(el) { el.replaceChildren(h('p', { class: 'muted' }, 'Loading…')); }
function showError(el, e) { el.replaceChildren(h('p', { class: 'error' }, 'Could not load: ' + e.message)); console.error(e); }
function empty(msg) { return h('div', { class: 'empty' }, msg); }

const tip = h('div', { class: 'tip', hidden: true });
document.body.append(tip);
function showTip(e, text) { tip.textContent = text; tip.hidden = false; tip.style.left = (e.clientX + 12) + 'px'; tip.style.top = (e.clientY + 12) + 'px'; }
function hideTip() { tip.hidden = true; }

// ---------- charts ----------

function observeWidth(el, f) {
  const ro = new ResizeObserver(() => f(el.clientWidth));
  ro.observe(el);
  onCleanup(() => ro.disconnect());
}

/* chart draws a uPlot time series.
   series: [{label, color, unit, dec, bars, scale, values, fill, dash, points, value(fmt)}]; data = [xs, ...values]. */
function chart(el, { xs, series, height = 240, sync, xTime = true, xFmt, bands, legend = true, yRange, hooks, axisFmt, incrs }) {
  const muted = cssVar('--muted'), line = cssVar('--line');
  const scales = { x: { time: xTime } };
  const axes = [{ stroke: muted, grid: { stroke: line, width: 1 }, ticks: { stroke: line }, values: xFmt }];
  const useries = [{ label: xTime ? 'Time' : '', value: xTime ? (u, v) => v == null ? '' : fmtDateTime(v) : (u, v) => v == null ? '' : (xFmt ? xFmt(u, [v])[0] : v) }];
  const seen = [];
  for (const sr of series) {
    const sc = sr.scale || 'y';
    if (!scales[sc]) {
      const n = seen.push(sc);
      scales[sc] = { auto: true, dir: sr.invert ? -1 : 1 };
      if (yRange || sr.bars || sr.zero) scales[sc].range = yRange || ((u, mn, mx) => [Math.min(0, mn ?? 0), (mx ?? 1) > 0 ? mx * 1.08 : 1]);
      axes.push({
        scale: sc, side: n % 2 ? 3 : 1, stroke: seen.length > 1 || series.length > 1 ? (sr.axisColor || sr.color) : muted,
        grid: { show: n === 1, stroke: line, width: 1 }, ticks: { stroke: line }, size: 60, incrs: sr.incrs || (n === 1 ? incrs : undefined),
        values: (u, vals) => vals.map(v => (sr.axisFmt || axisFmt || fmtCompact)(v)),
      });
    }
    useries.push({
      label: sr.label, stroke: sr.color, scale: sc, width: sr.bars ? 0 : (sr.width ?? 2), dash: sr.dash,
      fill: sr.bars ? sr.color : sr.fill, spanGaps: sr.spanGaps ?? true, show: sr.show ?? true,
      paths: sr.bars ? uPlot.paths.bars({ size: [0.75, 48], align: 1 }) : sr.stepped ? uPlot.paths.stepped({ align: 1 }) : undefined,
      points: { show: sr.points ?? false, size: 5, fill: sr.color },
      value: sr.value || ((u, v) => v == null ? '—' : fmtNum(v, sr.dec ?? 1) + (sr.unit ? ' ' + sr.unit : '')),
    });
  }
  const opts = {
    width: el.clientWidth || 600, height, scales, axes, series: useries, bands,
    legend: { show: legend, live: true }, cursor: { sync: sync ? { key: sync, setSeries: false } : undefined, drag: { x: true, y: false } },
    hooks,
  };
  const u = new uPlot(opts, [xs, ...series.map(sr => sr.values)], el);
  observeWidth(el, w => u.setSize({ width: w, height }));
  onCleanup(() => u.destroy());
  return u;
}

/* stackedBars draws stacked bars with uPlot by plotting running totals, tallest first, so each bar covers the ones
   above it; the legend shows each part's own value. */
function stackedBars(el, { xs, stacks, height = 240, unit = '', dec = 0, axisFmt, valueFmt, xFmt, sync, incrs }) {
  const totals = xs.map(() => 0);
  const cum = stacks.map(st => st.values.map((v, i) => (totals[i] += v || 0)));
  const fmt = valueFmt || (v => fmtNum(v, dec) + (unit ? ' ' + unit : ''));
  const series = stacks.map((st, k) => ({
    label: st.label, color: st.color, bars: true, values: cum[k], scale: 'y',
    value: (u, v, si, i) => i == null ? '—' : fmt(st.values[i] || 0),
  })).reverse();
  return chart(el, { xs, series, height, axisFmt: axisFmt || fmtCompact, xFmt, sync, incrs, yRange: (u, mn, mx) => [0, (mx || 1) * 1.08] });
}

function sparkline(values, { w = 190, h: ht = 38, color = cssVar('--accent'), bars = false } = {}) {
  const pts = values.map((v, i) => [i, v]).filter(p => p[1] != null && isFinite(p[1]));
  const svg = s('svg', { width: '100%', height: ht, viewBox: `0 0 ${w} ${ht}`, preserveAspectRatio: 'none' });
  if (pts.length < 2) return svg;
  const ys = pts.map(p => p[1]);
  let mn = Math.min(...ys), mx = Math.max(...ys);
  if (bars) mn = 0;
  if (mx === mn) { mx += 1; mn -= 1; }
  const X = i => i / (values.length - 1) * (w - 2) + 1, Y = v => ht - 2 - (v - mn) / (mx - mn) * (ht - 4);
  if (bars) {
    const bw = Math.max(1, (w / values.length) * 0.7);
    for (const [i, v] of pts) svg.append(s('rect', { x: X(i) - bw / 2, y: Y(v), width: bw, height: ht - 2 - Y(v), fill: color, opacity: .75 }));
  } else {
    svg.append(s('path', { d: pts.map((p, k) => (k ? 'L' : 'M') + X(p[0]).toFixed(1) + ',' + Y(p[1]).toFixed(1)).join(''), fill: 'none', stroke: color, 'stroke-width': 1.8, 'vector-effect': 'non-scaling-stroke' }));
  }
  return svg;
}

function rings(a, size = 120) {
  const svg = s('svg', { width: size, height: size, viewBox: '0 0 120 120' });
  const defs = [[a?.move, a?.moveGoal, '--move', 52], [a?.exercise, a?.exerciseGoal, '--exercise', 38], [a?.stand, a?.standGoal, '--stand', 24]];
  for (const [v, goal, color, r] of defs) {
    const c = cssVar(color), len = 2 * Math.PI * r, f = goal > 0 ? (v || 0) / goal : 0;
    svg.append(s('circle', { cx: 60, cy: 60, r, fill: 'none', stroke: c, 'stroke-opacity': .2, 'stroke-width': 12 }));
    // Past 100 % the ring keeps going round, a darker lap on top.
    svg.append(s('circle', { cx: 60, cy: 60, r, fill: 'none', stroke: c, 'stroke-width': 12, 'stroke-linecap': 'round',
      'stroke-dasharray': `${Math.min(f, 1) * len} ${len}`, transform: 'rotate(-90 60 60)' }));
    if (f > 1) svg.append(s('circle', { cx: 60, cy: 60, r, fill: 'none', stroke: c, 'stroke-width': 12, 'stroke-linecap': 'round', filter: 'brightness(.75)',
      'stroke-dasharray': `${Math.min(f - 1, 1) * len} ${len}`, transform: 'rotate(-90 60 60)' }));
  }
  return svg;
}

function seg(options, value, onChange) {
  const el = h('div', { class: 'seg' });
  for (const [v, label] of options) {
    el.append(h('button', { class: v === value ? 'on' : '', onclick: () => {
      for (const b of el.children) b.classList.remove('on');
      el.querySelector(`[data-v="${CSS.escape(v)}"]`).classList.add('on');
      onChange(v);
    }, 'data-v': v }, label));
  }
  return el;
}

function card(title, ...children) { return h('section', { class: 'card' }, title ? h('h2', {}, title) : null, ...children); }
function stat(k, v, unit) { return h('div', { class: 'stat' }, h('div', { class: 'k' }, k), h('div', { class: 'v' }, v, unit ? h('small', {}, unit) : null)); }

// Linear trend per day over [t, v] points.
function slopePerDay(points) {
  const n = points.length;
  if (n < 3) return null;
  let sx = 0, sy = 0, sxx = 0, sxy = 0;
  for (const [t, v] of points) { const x = t / DAY; sx += x; sy += v; sxx += x * x; sxy += x * v; }
  const d = n * sxx - sx * sx;
  return d ? (n * sxy - sx * sy) / d : null;
}

let typeListPromise;
function typeList() { return typeListPromise ||= api('types').then(list => Object.fromEntries(list.map(t => [t.type, t]))); }

// ---------- router ----------

const pages = {};
function route() {
  const [path, query] = location.hash.replace(/^#\/?/, '').split('?');
  const [page, ...rest] = path.split('/');
  return { page: page || 'dashboard', arg: rest.join('/'), params: new URLSearchParams(query || '') };
}
function setParams(params) {
  const r = route();
  const q = new URLSearchParams(params).toString();
  history.replaceState(null, '', '#/' + (r.page === 'dashboard' ? '' : r.page) + (r.arg ? '/' + r.arg : '') + (q ? '?' + q : ''));
}
async function render() {
  resetPage();
  const r = route();
  const pageName = r.page === 'workout' ? 'workouts' : r.page;
  for (const a of document.querySelectorAll('#nav a')) a.classList.toggle('active', a.dataset.page === pageName);
  const main = $('#app');
  loading(main);
  window.scrollTo(0, 0);
  const f = pages[r.page] || pages.dashboard;
  try { await f(main, r.arg, r.params); } catch (e) { showError(main, e); }
}
window.addEventListener('hashchange', render);

async function refreshStatus() {
  try {
    const st = await api('status', {}, { cache: false });
    const el = $('#sync-status');
    if (st.lastSync?.started) el.textContent = `Last sync ${fmtAgo(st.lastSync.finished || st.lastSync.started)}` + (st.lastSync.device ? ` from ${st.lastSync.device}` : '');
    else el.textContent = 'No sync yet';
    window.healthStatus = st;
  } catch { /* shown by the page */ }
}

// ---------- dashboard ----------

const DASH_KPIS = ['step_count', 'active_energy_burned', 'apple_exercise_time', 'distance_walking_running', 'flights_climbed',
  'resting_heart_rate', 'heart_rate_variability_sdnn', 'walking_heart_rate_average', 'vo2_max', 'body_mass', 'respiratory_rate',
  'oxygen_saturation', 'time_in_daylight', 'apple_sleeping_wrist_temperature'];

pages.dashboard = async (main) => {
  const types = await typeList();
  main.replaceChildren();
  const top = h('div', { class: 'grid cols-2' });
  const ringsCard = card('Activity rings');
  const sleepCard = card('Last night');
  top.append(ringsCard, sleepCard);
  const kpis = h('div', { class: 'grid kpis' });
  const workoutsCard = card('Recent workouts');
  const weeklyCard = card('Training volume, last 12 weeks');
  main.append(h('h1', {}, 'Dashboard'), top, h('div', { style: { height: '16px' } }), kpis, h('div', { class: 'grid cols-2' }, workoutsCard, weeklyCard));

  // Rings: today big, the week small.
  api('activity', { from: daysAgo(6), to: today() }).then(days => {
    const byDate = Object.fromEntries(days.map(d => [d.date, d]));
    const t = byDate[today()] || days[days.length - 1];
    const row = h('div', { class: 'rings-row' });
    for (let i = 6; i >= 0; i--) {
      const d = daysAgo(i);
      row.append(h('div', { class: 'ring-day' }, rings(byDate[d], 44), h('div', {}, new Date(d + 'T12:00').toLocaleDateString(undefined, { weekday: 'narrow' }))));
    }
    if (!t) { ringsCard.append(empty('No activity summaries yet.')); return; }
    const line = (label, v, goal, unit, color) => h('div', {}, h('div', { class: 'small muted' }, label),
      h('div', { style: { fontSize: '20px', fontWeight: 700, color: cssVar(color) } }, `${fmtNum(v, 0)}/${fmtNum(goal, 0)}`, h('small', { class: 'muted' }, ' ' + unit)));
    ringsCard.append(h('div', { class: 'row', style: { gap: '24px' } }, rings(t, 130),
      h('div', { class: 'grid', style: { gap: '8px' } }, line('Move', t.move, t.moveGoal, 'kcal', '--move'),
        line('Exercise', t.exercise, t.exerciseGoal, 'min', '--exercise'), line('Stand', t.stand, t.standGoal, 'h', '--stand')),
      row), h('p', { class: 'small muted' }, t.date === today() ? 'Today' : 'Latest day: ' + t.date));
  }).catch(e => ringsCard.append(h('p', { class: 'error' }, e.message)));

  // Sleep
  api('sleep', { from: daysAgo(14), to: today() }).then(nights => {
    if (!nights.length) { sleepCard.append(empty('No sleep data in the last two weeks.')); return; }
    const last = nights[nights.length - 1];
    const avg = nights.reduce((a, n) => a + n.asleep, 0) / nights.length;
    sleepCard.append(h('div', { class: 'stats' },
      stat('Asleep', fmtHM(last.asleep)), stat('Bedtime', last.start ? fmtTime(last.start) : '—'), stat('Woke up', last.end ? fmtTime(last.end) : '—'),
      stat('Deep', fmtHM(last.stages.deep)), stat('REM', fmtHM(last.stages.rem)), stat('14-night average', fmtHM(avg))),
    h('div', { style: { marginTop: '10px' } }, sparkline(nights.map(n => n.asleep / 60), { bars: true, color: '#8b5cf6', h: 44 })),
    h('p', { class: 'small muted' }, `Night of ${last.date} · `, h('a', { href: '#/sleep' }, 'Sleep details')));
  }).catch(e => sleepCard.append(h('p', { class: 'error' }, e.message)));

  // KPI cards
  const shown = DASH_KPIS.filter(t => types[t]);
  if (!shown.length) kpis.append(card(null, empty('No samples yet. Sync from the iPhone app and they show up here.')));
  for (const t of shown) {
    const info = types[t];
    const el = h('section', { class: 'card kpi', onclick: () => location.hash = '#/metrics?types=' + t + '&range=90d' },
      h('div', { class: 'label' }, typeLabel(t)), h('div', { class: 'value' }, '…'));
    kpis.append(el);
    api('series', { type: t, from: daysAgo(59), to: today(), bucket: 'day' }).then(sr => {
      const k = scaleFor(sr.unit), pts = sr.points;
      if (!pts.length) { el.querySelector('.value').textContent = '—'; return; }
      const byDay = new Map(pts.map(p => [isoDate(new Date(p.t * 1000)), p.v * k]));
      const vals = []; for (let i = 59; i >= 0; i--) vals.push(byDay.get(daysAgo(i)) ?? null);
      const last = pts[pts.length - 1];
      const avg = arr => { const xs = arr.filter(v => v != null); return xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : null; };
      const recent = avg(vals.slice(-7)), before = avg(vals.slice(-35, -7));
      const unit = prettyUnit(info.unit === '%' ? '%' : sr.unit);
      const headline = info.cumulative ? (byDay.get(today()) ?? 0) : last.v * k;
      const delta = recent != null && before ? (recent - before) / Math.abs(before) * 100 : null;
      const lowerIsBetter = ['resting_heart_rate', 'walking_heart_rate_average', 'respiratory_rate'].includes(t);
      const cls = delta == null || Math.abs(delta) < 2 ? 'flat' : (delta > 0) !== lowerIsBetter ? 'up' : 'down';
      el.querySelector('.value').replaceChildren(fmtNum(headline, typeDec(t)), h('small', {}, unit));
      el.append(h('div', { class: 'sub' }, info.cumulative ? 'today' : (isoDate(new Date(last.t * 1000)) === today() ? 'today' : fmtDateShort(last.t)),
        ' · 7-day avg ', fmtNum(recent, typeDec(t)), delta != null ? h('span', { class: cls }, ` ${delta > 0 ? '▲' : '▼'} ${fmtNum(Math.abs(delta), 0)}%`) : null),
      sparkline(vals, { bars: info.cumulative }));
    }).catch(e => el.append(h('div', { class: 'sub error' }, e.message)));
  }

  // Workouts
  api('workouts', { from: daysAgo(83), to: today() }).then(ws => {
    if (!ws.length) { workoutsCard.append(empty('No workouts in the last 12 weeks.')); weeklyCard.append(empty('Nothing to show yet.')); return; }
    workoutsCard.append(workoutTable(ws.slice(0, 8), { compact: true }), h('p', { class: 'small' }, h('a', { href: '#/workouts' }, 'All workouts →')));
    const chartEl = h('div', { class: 'chart' });
    weeklyCard.append(chartEl);
    weeklyChart(chartEl, ws, 12);
  }).catch(e => workoutsCard.append(h('p', { class: 'error' }, e.message)));
};

function mondayOf(t) { const d = new Date(t * 1000); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - (d.getDay() + 6) % 7); return d; }

// weeklyChart: workout minutes per week, stacked by activity type.
function weeklyChart(el, ws, weeks, { height = 240, monthly = false } = {}) {
  const startOf = monthly ? (t => { const d = new Date(t * 1000); return new Date(d.getFullYear(), d.getMonth(), 1); }) : mondayOf;
  const end = startOf(Date.now() / 1000);
  const xs = [];
  for (let i = weeks - 1; i >= 0; i--) {
    const d = new Date(end);
    if (monthly) d.setMonth(d.getMonth() - i); else d.setDate(d.getDate() - 7 * i);
    xs.push(d.getTime() / 1000);
  }
  const index = new Map(xs.map((x, i) => [x, i]));
  const byType = {};
  for (const w of ws) {
    const i = index.get(startOf(w.start).getTime() / 1000);
    if (i == null) continue;
    (byType[w.activityType] ||= xs.map(() => 0))[i] += w.duration / 60;
  }
  const order = Object.entries(byType).sort((a, b) => b[1].reduce((x, y) => x + y) - a[1].reduce((x, y) => x + y));
  const stacks = order.map(([t, values]) => ({ label: activityName(t), color: activityColor(t), values }));
  if (!stacks.length) { el.append(empty('No workouts in this period.')); return; }
  stackedBars(el, { xs, stacks, height, valueFmt: v => fmtHM(v), axisFmt: v => fmtCompact(v / 60) + 'h', incrs: MINUTE_INCRS });
}

function workoutTable(ws, { compact = false } = {}) {
  const tb = h('tbody');
  for (const w of ws) {
    const pace = w.distance > 0 && paceSports.has(w.activityType) ? fmtPace(w.duration / (w.distance / 1000)) : w.distance > 0 ? fmtSpeed(w.distance / w.duration) : '';
    tb.append(h('tr', { class: 'click', onclick: () => location.hash = '#/workout/' + w.uuid },
      h('td', {}, h('span', { class: 'chip', style: { color: activityColor(w.activityType), padding: '1px 8px' } }, h('span', { class: 'dot', style: { background: activityColor(w.activityType) } }), activityName(w.activityType)),
        w.hasRoute ? ' 📍' : ''),
      h('td', {}, compact ? fmtDate(w.start) : fmtDateTime(w.start)),
      h('td', { class: 'r num' }, fmtDuration(w.duration)),
      h('td', { class: 'r num' }, w.distance ? fmtDist(w.distance) : ''),
      compact ? null : h('td', { class: 'r num' }, pace),
      h('td', { class: 'r num' }, w.energy ? fmtKcal(w.energy) : ''),
      h('td', { class: 'r num' }, w.hrAvg ? fmtNum(w.hrAvg, 0) + ' bpm' : ''),
      compact ? null : h('td', { class: 'r num' }, w.elevation ? fmtElev(w.elevation) : ''),
      compact ? null : h('td', { class: 'muted' }, w.source)));
  }
  return h('div', { class: 'scroll' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Workout'), h('th', {}, 'Date'), h('th', { class: 'r' }, 'Time'),
    h('th', { class: 'r' }, 'Distance'), compact ? null : h('th', { class: 'r' }, 'Pace / speed'), h('th', { class: 'r' }, 'Energy'), h('th', { class: 'r' }, 'Avg HR'),
    compact ? null : h('th', { class: 'r' }, 'Climb'), compact ? null : h('th', {}, 'Source'))), tb));
}

// ---------- metrics explorer ----------

const RANGES = [['7d', '7 days', 7], ['30d', '30 days', 30], ['90d', '90 days', 90], ['1y', '1 year', 365], ['2y', '2 years', 730], ['5y', '5 years', 1826], ['all', 'All', 0]];

pages.metrics = async (main, _, params) => {
  const types = await typeList();
  const state = {
    types: (params.get('types') || 'resting_heart_rate,heart_rate_variability_sdnn').split(',').filter(t => types[t]),
    range: params.get('range') || '90d', bucket: params.get('bucket') || 'auto', mode: params.get('mode') || 'stack',
    from: params.get('from') || '', to: params.get('to') || '', band: params.get('band') !== '0',
  };
  const save = () => setParams(Object.fromEntries(Object.entries({ types: state.types.join(','), range: state.range, bucket: state.bucket,
    mode: state.mode, from: state.range === 'custom' ? state.from : '', to: state.range === 'custom' ? state.to : '', band: state.band ? '' : '0' }).filter(([, v]) => v)));

  const all = Object.values(types).filter(t => t.kind === 'quantity' || t.kind === 'category').sort((a, b) => typeLabel(a.type).localeCompare(typeLabel(b.type)));
  const chips = h('div', { class: 'row' });
  const search = h('input', { list: 'type-options', placeholder: 'Add a metric…', style: { minWidth: '240px' } });
  const datalist = h('datalist', { id: 'type-options' }, all.map(t => h('option', { value: typeLabel(t.type) + ' · ' + t.type })));
  search.addEventListener('change', () => {
    const t = search.value.split(' · ').pop().trim();
    search.value = '';
    if (types[t] && !state.types.includes(t)) { state.types.push(t); draw(); }
  });
  const quick = h('div', { class: 'type-picker' }, COMMON_TYPES.filter(t => types[t]).map(t => h('span', { class: 'chip', onclick: () => {
    state.types = state.types.includes(t) ? state.types.filter(x => x !== t) : [...state.types, t]; draw();
  } }, typeLabel(t))));

  const fromIn = h('input', { type: 'date', value: state.from || daysAgo(90) });
  const toIn = h('input', { type: 'date', value: state.to || today() });
  const customRow = h('span', { class: 'row', hidden: state.range !== 'custom' }, fromIn, '–', toIn,
    h('button', { onclick: () => { state.from = fromIn.value; state.to = toIn.value; draw(); } }, 'Apply'));
  const bucketSel = h('select', { onchange: e => { state.bucket = e.target.value; draw(); } },
    [['auto', 'Auto'], ['raw', 'Every sample'], ['hour', 'Hourly'], ['day', 'Daily'], ['week', 'Weekly'], ['month', 'Monthly'], ['year', 'Yearly']]
      .map(([v, l]) => h('option', { value: v, selected: v === state.bucket }, l)));
  const bandBox = h('label', { class: 'row small', style: { gap: '4px' } }, h('input', { type: 'checkbox', checked: state.band, onchange: e => { state.band = e.target.checked; draw(); } }), 'min/max band');

  const out = h('div');
  const summary = h('div');
  main.replaceChildren(h('h1', {}, 'Metrics'),
    h('section', { class: 'card' },
      h('div', { class: 'toolbar' }, chips),
      h('div', { class: 'toolbar' }, search, datalist, h('span', { class: 'muted small' }, 'or pick:')), quick,
      h('div', { class: 'toolbar', style: { marginTop: '12px', marginBottom: 0 } },
        seg([...RANGES.map(r => [r[0], r[1]]), ['custom', 'Custom']], state.range, v => { state.range = v; customRow.hidden = v !== 'custom'; if (v !== 'custom') draw(); }),
        customRow, bucketSel,
        seg([['stack', 'Stacked'], ['overlay', 'Overlay']], state.mode, v => { state.mode = v; draw(); }), bandBox)),
    h('div', { style: { height: '16px' } }), out, summary);

  let drawSeq = 0;
  async function draw() {
    save();
    const seq = ++drawSeq;
    resetPage();
    chips.replaceChildren(...state.types.map((t, i) => h('span', { class: 'chip on', style: { color: PALETTE[i % PALETTE.length] } },
      h('span', { class: 'dot', style: { background: PALETTE[i % PALETTE.length] } }), typeLabel(t),
      h('span', { class: 'x', title: 'Remove', onclick: () => { state.types = state.types.filter(x => x !== t); draw(); } }, '✕'))));
    for (const c of quick.children) c.classList.toggle('on', state.types.some(t => typeLabel(t) === c.textContent));
    bandBox.hidden = state.mode !== 'stack';
    if (!state.types.length) { out.replaceChildren(card(null, empty('Pick one or more metrics above.'))); summary.replaceChildren(); return; }
    loading(out);
    let from, to = today();
    if (state.range === 'custom') { from = state.from || daysAgo(90); to = state.to || today(); }
    else if (state.range === 'all') {
      const first = Math.min(...state.types.map(t => types[t].first));
      from = isoDate(new Date(first * 1000));
    } else from = daysAgo(RANGES.find(r => r[0] === state.range)[2] - 1);
    let results;
    try {
      results = await Promise.all(state.types.map(t => api('series', { type: t, from, to, bucket: state.bucket })));
    } catch (e) { if (seq === drawSeq) showError(out, e); return; }
    if (seq !== drawSeq) return;
    out.replaceChildren();
    const defs = results.map((r, i) => {
      const k = scaleFor(r.unit), unit = prettyUnit(r.unit);
      return { r, i, k, unit, color: PALETTE[i % PALETTE.length], t: state.types[i], bars: r.cumulative && r.bucket !== 'raw' && r.bucket !== 'hour' || (r.cumulative && r.bucket === 'hour') };
    });
    const bucketLabel = results[0]?.bucket;
    if (state.mode === 'overlay') {
      const tables = defs.map(d => [d.r.points.map(p => p.t), d.r.points.map(p => p.v * d.k)]);
      const joined = tables.length > 1 ? uPlot.join(tables) : tables[0];
      const el = h('div', { class: 'chart' });
      out.append(card(`${defs.map(d => typeLabel(d.t)).join(' · ')}`, el, h('p', { class: 'small muted' }, `Grouped by ${bucketLabel}. Each unit has its own axis.`)));
      chart(el, { xs: joined[0], height: 420, series: defs.map((d, j) => ({
        label: typeLabel(d.t), color: d.color, unit: d.unit, dec: typeDec(d.t), scale: d.unit || d.t, values: joined[j + 1],
        bars: false, points: d.r.points.length < 60, fill: d.bars ? d.color + '22' : undefined, stepped: d.bars && defs.length === 1,
      })) });
    } else {
      for (const d of defs) {
        const el = h('div', { class: 'chart' });
        const pts = d.r.points;
        out.append(card(null, h('div', { class: 'row spread' }, h('h2', { style: { color: d.color } }, typeLabel(d.t)), h('span', { class: 'muted small' }, `${d.unit || 'count'} · ${d.r.bucket}${d.r.cumulative ? ' totals' : ' averages'}`)), el));
        if (!pts.length) { el.append(empty('No data in this range.')); continue; }
        const xs = pts.map(p => p.t), ys = pts.map(p => p.v * d.k);
        const series = [{ label: typeLabel(d.t), color: d.color, unit: d.unit, dec: typeDec(d.t), values: ys, bars: d.bars, points: !d.bars && pts.length < 90 }];
        let bands;
        if (state.band && !d.r.cumulative && pts.some(p => p.min != null && p.n > 1)) {
          series.push({ label: 'max', color: d.color + '55', width: 1, unit: d.unit, dec: typeDec(d.t), values: pts.map(p => p.max != null ? p.max * d.k : null) });
          series.push({ label: 'min', color: d.color + '55', width: 1, unit: d.unit, dec: typeDec(d.t), values: pts.map(p => p.min != null ? p.min * d.k : null) });
          bands = [{ series: [2, 3], fill: d.color + '1f' }];
        }
        chart(el, { xs, series, bands, height: defs.length > 2 ? 200 : 260, sync: 'metrics' });
      }
    }
    // Summary table
    const tb = h('tbody');
    for (const d of defs) {
      const pts = d.r.points, vals = pts.map(p => p.v * d.k);
      if (!vals.length) { tb.append(h('tr', {}, h('td', {}, typeLabel(d.t)), h('td', { colspan: 6, class: 'muted' }, 'no data'))); continue; }
      const avg = vals.reduce((a, b) => a + b, 0) / vals.length;
      const mn = Math.min(...pts.map(p => (p.min ?? p.v) * d.k)), mx = Math.max(...pts.map(p => (p.max ?? p.v) * d.k));
      const slope = slopePerDay(pts.map((p, i) => [p.t, vals[i]]));
      const dec = typeDec(d.t);
      tb.append(h('tr', {}, h('td', {}, h('span', { class: 'dot', style: { background: d.color, display: 'inline-block', width: '9px', height: '9px', borderRadius: '50%', marginRight: '6px' } }), typeLabel(d.t)),
        h('td', { class: 'r num' }, fmtNum(avg, dec), ' ', d.unit), h('td', { class: 'r num' }, fmtNum(mn, dec)), h('td', { class: 'r num' }, fmtNum(mx, dec)),
        h('td', { class: 'r num' }, d.r.cumulative ? fmtNum(vals.reduce((a, b) => a + b, 0), dec) : '—'),
        h('td', { class: 'r num' }, fmtNum(vals[vals.length - 1], dec)),
        h('td', { class: 'r num' }, slope == null ? '—' : (slope > 0 ? '+' : '') + fmtNum(slope * 30, dec + 1) + ' / 30 days')));
    }
    summary.replaceChildren(card('Summary', h('div', { class: 'scroll' }, h('table', {}, h('thead', {}, h('tr', {}, ...['Metric', `Average per ${bucketLabel === 'raw' ? 'sample' : bucketLabel}`, 'Min', 'Max', 'Total', 'Latest', 'Trend'].map((x, i) => h('th', { class: i ? 'r' : '' }, x)))), tb))));
  }
  draw();
};

// ---------- workouts ----------

pages.workouts = async (main, _, params) => {
  const state = { type: params.get('type') || '', range: params.get('range') || '365' };
  const [all, profile] = await Promise.all([api('workouts'), api('profile').catch(() => ({}))]);
  const typesCount = {};
  for (const w of all) typesCount[w.activityType] = (typesCount[w.activityType] || 0) + 1;
  const years = [...new Set(all.map(w => new Date(w.start * 1000).getFullYear()))].sort((a, b) => b - a);

  const body = h('div');
  const typeSel = h('select', { onchange: e => { state.type = e.target.value; draw(); } }, h('option', { value: '' }, `All activities (${all.length})`),
    Object.entries(typesCount).sort((a, b) => b[1] - a[1]).map(([t, n]) => h('option', { value: t, selected: t === state.type }, `${activityName(t)} (${n})`)));
  const rangeSel = h('select', { onchange: e => { state.range = e.target.value; draw(); } },
    [['90', 'Last 90 days'], ['365', 'Last 12 months'], ...years.map(y => ['y' + y, String(y)]), ['all', 'All time']]
      .map(([v, l]) => h('option', { value: v, selected: v === state.range }, l)));
  main.replaceChildren(h('h1', {}, 'Workouts'), h('div', { class: 'toolbar' }, typeSel, rangeSel), body);
  if (!all.length) { body.append(card(null, empty('No workouts yet.'))); return; }

  function draw() {
    resetPage();
    setParams(Object.fromEntries(Object.entries(state).filter(([, v]) => v)));
    let from = 0, to = Infinity;
    const now = Date.now() / 1000;
    if (state.range === 'all') from = 0;
    else if (state.range.startsWith('y')) { const y = +state.range.slice(1); from = new Date(y, 0, 1) / 1000; to = new Date(y + 1, 0, 1) / 1000; }
    else from = now - +state.range * DAY;
    const ws = all.filter(w => (!state.type || w.activityType === state.type) && w.start >= from && w.start < to);
    const sum = (f) => ws.reduce((a, w) => a + (f(w) || 0), 0);

    const totals = card(null, h('div', { class: 'stats' }, stat('Workouts', fmtNum(ws.length)), stat('Time', fmtHM(sum(w => w.duration) / 60)),
      stat('Distance', ...distParts(sum(w => w.distance))), stat('Energy', fmtNum(sum(w => w.energy)), 'kcal'),
      stat('Climbing', fmtElev(sum(w => w.elevation))), stat('Avg per week', fmtHM(sum(w => w.duration) / 60 / Math.max(1, ((Math.min(to, now) - Math.max(from, ws.length ? ws[ws.length - 1].start : from)) / (7 * DAY)))))));

    const calEl = h('div', { class: 'calendar' });
    const volEl = h('div', { class: 'chart' });
    const loadEl = h('div', { class: 'chart' });
    const spanDays = (Math.min(to, now) - (from || (all.length ? all[all.length - 1].start : now))) / DAY;
    const monthly = spanDays > 400;
    body.replaceChildren(totals,
      card('Calendar', calEl, h('p', { class: 'small muted' }, 'Each square is a day, darker for more workout time.')),
      h('div', { class: 'grid cols-2' }, card(monthly ? 'Time per month' : 'Time per week', volEl),
        card('Training load', loadEl, h('p', { class: 'small muted' },
          `Fitness is the 42-day and fatigue the 7-day weighted average of daily TRIMP (heart-rate based effort, max HR ${profile.maxHr || '?'}, resting ${profile.restingHr || '?'}). Form = fitness − fatigue. Workouts without heart rate are skipped.`))),
      card(`${ws.length} workouts`, workoutTable(ws.slice(0, 300)), ws.length > 300 ? h('p', { class: 'small muted' }, 'Showing the latest 300; narrow the range to see others.') : null));

    calendar(calEl, ws, state.range.startsWith('y') ? new Date(+state.range.slice(1), 11, 31) : new Date());
    const periods = monthly ? Math.ceil(spanDays / 30.4) + 1 : Math.max(4, Math.ceil(Math.min(spanDays, 3650) / 7) + 1);
    if (state.range.startsWith('y')) weeklyChartRange(volEl, ws, from, to, monthly);
    else weeklyChart(volEl, ws, Math.min(periods, monthly ? 240 : 60), { monthly });
    trainingLoad(loadEl, all.filter(w => !state.type || w.activityType === state.type), profile, from, Math.min(to, now));
  }
  draw();
};

function weeklyChartRange(el, ws, from, to, monthly) {
  // Weeks of a past year: the same as weeklyChart but anchored at the end of that year.
  const startOf = monthly ? (t => { const d = new Date(t * 1000); return new Date(d.getFullYear(), d.getMonth(), 1); }) : mondayOf;
  const xs = [];
  for (let d = startOf(from); d / 1000 < to; monthly ? d.setMonth(d.getMonth() + 1) : d.setDate(d.getDate() + 7)) xs.push(d.getTime() / 1000);
  const index = new Map(xs.map((x, i) => [x, i]));
  const byType = {};
  for (const w of ws) {
    const i = index.get(startOf(w.start).getTime() / 1000);
    if (i != null) (byType[w.activityType] ||= xs.map(() => 0))[i] += w.duration / 60;
  }
  const stacks = Object.entries(byType).map(([t, values]) => ({ label: activityName(t), color: activityColor(t), values }));
  if (!stacks.length) { el.append(empty('No workouts.')); return; }
  stackedBars(el, { xs, stacks, valueFmt: v => fmtHM(v), axisFmt: v => fmtCompact(v / 60) + 'h', incrs: MINUTE_INCRS });
}

function calendar(el, ws, endDate) {
  const minutes = {};
  for (const w of ws) { const d = isoDate(new Date(w.start * 1000)); minutes[d] = (minutes[d] || 0) + w.duration / 60; }
  const end = new Date(endDate); end.setHours(0, 0, 0, 0);
  const start = new Date(end); start.setDate(start.getDate() - 364); start.setDate(start.getDate() - (start.getDay() + 6) % 7);
  const cell = 13, gap = 3, weeks = Math.ceil(((end - start) / 864e5 + 1) / 7);
  const svg = s('svg', { width: weeks * (cell + gap) + 30, height: 7 * (cell + gap) + 20 });
  const accent = cssVar('--accent'), lineC = cssVar('--line');
  const max = Math.max(60, ...Object.values(minutes));
  let lastMonth = -1;
  for (let d = new Date(start), i = 0; d <= end; d.setDate(d.getDate() + 1), i++) {
    const wk = Math.floor(i / 7), dow = (d.getDay() + 6) % 7, key = isoDate(d), m = minutes[key] || 0;
    if (d.getMonth() !== lastMonth && dow === 0) {
      lastMonth = d.getMonth();
      svg.append(s('text', { x: 30 + wk * (cell + gap), y: 10, 'font-size': 10, fill: cssVar('--muted') }, d.toLocaleDateString(undefined, { month: 'short' })));
    }
    const label = `${weekdayFmt.format(d)}: ${m ? fmtHM(m) : 'no workouts'}`;
    svg.append(s('rect', { x: 30 + wk * (cell + gap), y: 16 + dow * (cell + gap), width: cell, height: cell, rx: 2,
      fill: m ? accent : lineC, 'fill-opacity': m ? 0.25 + 0.75 * Math.min(1, m / max) : 1,
      onmousemove: e => showTip(e, label), onmouseleave: hideTip }));
  }
  ['Mon', 'Wed', 'Fri'].forEach((d, i) => svg.append(s('text', { x: 0, y: 26 + i * 2 * (cell + gap), 'font-size': 10, fill: cssVar('--muted') }, d)));
  el.replaceChildren(svg);
}

// Banister TRIMP per day, then exponentially weighted fitness (42 days) and fatigue (7 days).
function trainingLoad(el, ws, profile, from, to) {
  const maxHr = +settings.maxHr || profile.maxHr, rest = profile.restingHr || 60;
  if (!maxHr) { el.append(empty('Needs a maximum heart rate (Settings) or your date of birth.')); return; }
  const male = profile.characteristics?.biologicalSex === '2';
  const daily = new Map();
  let first = Infinity;
  for (const w of ws) {
    if (!w.hrAvg) continue;
    const hrr = Math.max(0, Math.min(1, (w.hrAvg - rest) / (maxHr - rest)));
    const trimp = (w.duration / 60) * hrr * 0.64 * Math.exp((male ? 1.92 : 1.67) * hrr);
    const day = Math.floor(new Date(isoDate(new Date(w.start * 1000)) + 'T00:00').getTime() / 1000);
    daily.set(day, (daily.get(day) || 0) + trimp);
    first = Math.min(first, day);
  }
  if (!daily.size) { el.append(empty('No workouts with heart rate.')); return; }
  const xs = [], fit = [], fat = [], form = [], load = [];
  let ctl = 0, atl = 0;
  const startDay = new Date(first * 1000); startDay.setHours(0, 0, 0, 0);
  for (let d = startDay; d.getTime() / 1000 <= to; d.setDate(d.getDate() + 1)) {
    const t = d.getTime() / 1000, x = daily.get(t) || 0;
    ctl += (x - ctl) / 42; atl += (x - atl) / 7;
    if (t >= from - DAY) { xs.push(t); fit.push(ctl); fat.push(atl); form.push(ctl - atl); load.push(x || null); }
  }
  if (!xs.length) { el.append(empty('No data in this range.')); return; }
  chart(el, { xs, height: 240, series: [
    { label: 'Fitness', color: '#2563eb', dec: 0, values: fit },
    { label: 'Fatigue', color: '#e8445a', dec: 0, values: fat },
    { label: 'Form', color: '#16a34a', dec: 0, values: form, dash: [4, 3] },
    { label: 'Daily TRIMP', color: '#9aa1ad88', dec: 0, values: load, bars: true, scale: 'trimp', axisColor: cssVar('--muted') },
  ] });
}

// ---------- one workout ----------

pages.workout = async (main, id) => {
  const w = await api('workouts/' + encodeURIComponent(id));
  const name = activityName(w.activityType);
  const isPace = paceSports.has(w.activityType);
  const statsByType = Object.fromEntries((w.statistics || []).map(s => [s.type, s]));
  const statsEl = h('div', { class: 'stats' },
    stat('Duration', fmtDuration(w.duration)),
    w.distance ? stat('Distance', ...distParts(w.distance)) : null,
    w.distance && isPace ? stat('Avg pace', fmtPace(w.duration / (w.distance / 1000))) : null,
    w.distance && !isPace ? stat('Avg speed', fmtSpeed(w.distance / w.duration)) : null,
    w.energy ? stat('Active energy', fmtNum(w.energy), 'kcal') : null,
    w.hrAvg ? stat('Avg heart rate', fmtNum(w.hrAvg), 'bpm') : null,
    w.hrMax ? stat('Max heart rate', fmtNum(w.hrMax), 'bpm') : null,
    (w.elevation || w.routeGain) ? stat('Climb', fmtElev(w.elevation ?? w.routeGain)) : null,
    statsByType.running_power ? stat('Avg power', fmtNum(statsByType.running_power.average), 'W') : null,
    statsByType.cycling_power ? stat('Avg power', fmtNum(statsByType.cycling_power.average), 'W') : null,
    statsByType.cycling_cadence ? stat('Avg cadence', fmtNum(statsByType.cycling_cadence.average), 'rpm') : null,
    statsByType.step_count && w.duration ? stat('Cadence', fmtNum(statsByType.step_count.sum / (w.duration / 60)), 'spm') : null,
    w.metadata?.HKAverageMETs ? stat('Avg METs', fmtNum(parseFloat(w.metadata.HKAverageMETs), 1)) : null,
    w.metadata?.HKWeatherTemperature ? stat('Weather', weatherText(w.metadata)) : null,
  );

  main.replaceChildren(
    h('p', { class: 'small' }, h('a', { href: '#/workouts' }, '← Workouts')),
    h('div', { class: 'row spread' }, h('h1', { style: { color: activityColor(w.activityType) } }, name),
      h('span', { class: 'muted' }, fmtDateTime(w.start), ' – ', fmtTime(w.end), ' · ', w.source, w.indoor ? h('span', { class: 'badge', style: { marginLeft: '6px' } }, 'Indoor') : null)),
    card(null, statsEl));

  const series = w.series || {};
  const route = w.route || [];
  let mapMarker, map, routeTimes = route.map(p => p[0]);

  if (route.length > 1) {
    const mapEl = h('div', { class: 'map' });
    let colorBy = 'speed';
    const legend = h('div', { class: 'legend' });
    const mapCard = card(null, h('div', { class: 'row spread', style: { marginBottom: '10px' } }, h('h2', { style: { margin: 0 } }, 'Route'),
      seg([['speed', isPace ? 'Pace' : 'Speed'], ['hr', 'Heart rate'], ['alt', 'Elevation'], ['plain', 'Plain']], colorBy, v => { colorBy = v; paint(); })), mapEl, legend);
    main.append(mapCard);
    map = makeMap(mapEl);
    const latlngs = route.map(p => [p[1], p[2]]);
    const speeds = smoothSpeed(route);
    let layer;
    function paint() {
      if (layer) layer.remove();
      layer = L.layerGroup().addTo(map);
      const vals = colorBy === 'speed' ? speeds : colorBy === 'hr' ? route.map(p => p[6] > 0 ? p[6] : null) : colorBy === 'alt' ? route.map(p => p[3]) : null;
      if (!vals || vals.every(v => v == null)) {
        L.polyline(latlngs, { color: activityColor(w.activityType), weight: 4 }).addTo(layer);
        legend.replaceChildren(colorBy !== 'plain' ? 'No data for this colouring.' : '');
      } else {
        const sorted = vals.filter(v => v != null).sort((a, b) => a - b);
        const lo = sorted[Math.floor(sorted.length * 0.05)], hi = sorted[Math.floor(sorted.length * 0.95)];
        const step = Math.max(1, Math.floor(route.length / 400));
        for (let i = 0; i < route.length - 1; i += step) {
          const j = Math.min(route.length - 1, i + step), v = vals[i];
          const f = v == null || hi === lo ? 0.5 : Math.max(0, Math.min(1, (v - lo) / (hi - lo)));
          L.polyline([latlngs[i], latlngs[j]], { color: rampColor(f), weight: 5, opacity: .95 }).addTo(layer);
        }
        const fmtV = colorBy === 'speed' ? (v => isPace ? fmtPace(1000 / v) : fmtSpeed(v)) : colorBy === 'hr' ? (v => fmtNum(v) + ' bpm') : fmtElev;
        legend.replaceChildren(h('span', {}, h('i', { style: { background: rampColor(0) } }), fmtV(lo)), h('span', {}, h('i', { style: { background: rampColor(.5) } }), 'middle'),
          h('span', {}, h('i', { style: { background: rampColor(1) } }), fmtV(hi)));
      }
      L.circleMarker(latlngs[0], { radius: 6, color: '#fff', weight: 2, fillColor: '#16a34a', fillOpacity: 1 }).addTo(layer).bindTooltip('Start');
      L.circleMarker(latlngs[latlngs.length - 1], { radius: 6, color: '#fff', weight: 2, fillColor: '#dc2626', fillOpacity: 1 }).addTo(layer).bindTooltip('Finish');
    }
    paint();
    map.fitBounds(L.latLngBounds(latlngs), { padding: [20, 20] });
    mapMarker = L.circleMarker(latlngs[0], { radius: 7, color: '#fff', weight: 2, fillColor: '#111', fillOpacity: 1, opacity: 0, fillOpacity: 0 }).addTo(map);
  }

  // Charts over time, synced with each other and with the map marker.
  const chartsCard = card('Over time');
  const hooks = { setCursor: [u => {
    if (!mapMarker || u.cursor.idx == null) return;
    const t = u.data[0][u.cursor.idx];
    let i = lowerBound(routeTimes, t);
    if (i >= route.length) i = route.length - 1;
    if (i >= 0 && Math.abs(routeTimes[i] - t) < 120) { mapMarker.setLatLng([route[i][1], route[i][2]]); mapMarker.setStyle({ opacity: 1, fillOpacity: 1 }); }
  }] };
  let charts = 0;
  const addChart = (title, xs, values, opts) => {
    if (!xs.length || values.every(v => v == null)) return;
    const el = h('div', { class: 'chart' });
    chartsCard.append(h('h3', {}, title), el);
    chart(el, { xs, height: 170, sync: 'workout', hooks, series: [{ label: title, values, ...opts }], xFmt: (u, vals) => vals.map(v => fmtDuration(v - w.start)), xTime: false });
    charts++;
  };
  const hr = series.heart_rate || [];
  addChart('Heart rate', hr.map(p => p[0]), hr.map(p => p[1]), { color: '#e8445a', unit: 'bpm', dec: 0, fill: '#e8445a18' });
  if (route.length > 1) {
    const sp = smoothSpeed(route);
    if (isPace) addChart('Pace', routeTimes, sp.map(v => v && v > 0.5 ? 1000 / v : null), { color: '#2563eb', invert: true, axisFmt: v => fmtPace(v).replace(/ \/.*/, ''), value: (u, v) => v == null ? '—' : fmtPace(v), fill: undefined });
    else addChart('Speed', routeTimes, sp.map(v => v == null ? null : imperial() ? v * 2.236936 : v * 3.6), { color: '#2563eb', unit: imperial() ? 'mph' : 'km/h', dec: 1 });
    addChart('Elevation', routeTimes, route.map(p => imperial() ? p[3] * 3.28084 : p[3]), { color: '#65a30d', unit: imperial() ? 'ft' : 'm', dec: 0, fill: '#65a30d22' });
  }
  for (const [t, label, unit, color, k] of [['running_power', 'Running power', 'W', '#f59e0b', 1], ['cycling_power', 'Power', 'W', '#f59e0b', 1],
    ['cycling_cadence', 'Cadence', 'rpm', '#8b5cf6', 1], ['running_stride_length', 'Stride length', 'm', '#8b5cf6', 1],
    ['running_vertical_oscillation', 'Vertical oscillation', 'cm', '#06b6d4', 1], ['running_ground_contact_time', 'Ground contact', 'ms', '#db2777', 1]]) {
    const sr = series[t];
    if (sr?.length > 2) addChart(label, sr.map(p => p[0]), sr.map(p => p[1] * k), { color, unit, dec: 0 });
  }
  if (charts) main.append(chartsCard);

  const grid = h('div', { class: 'grid cols-2' });
  // Splits
  const sp = (w.splits || {})[imperial() ? 'mi' : 'km'] || [];
  if (sp.length) {
    const unitM = imperial() ? 1609.344 : 1000;
    const paces = sp.map(x => x.seconds / (x.distance / unitM));
    const fastest = Math.min(...paces), slowest = Math.max(...paces);
    const tb = h('tbody');
    sp.forEach((x, i) => tb.append(h('tr', {},
      h('td', { class: 'num' }, x.distance < unitM * 0.98 ? fmtNum(x.distance / unitM, 2) : x.index),
      h('td', { class: 'num' }, isPace ? fmtDuration(paces[i]) : fmtSpeed(x.distance / x.seconds)),
      h('td', { style: { width: '40%' } }, h('div', { class: 'bar-bg' }, h('div', { class: 'bar', style: { width: (30 + 70 * (slowest === fastest ? 1 : (slowest - paces[i]) / (slowest - fastest))) + '%', background: paces[i] === fastest ? '#16a34a' : cssVar('--accent') } }))),
      h('td', { class: 'r num' }, x.hr ? fmtNum(x.hr) + ' bpm' : ''),
      h('td', { class: 'r num' }, fmtElev(x.gain)))));
    grid.append(card(`Splits per ${imperial() ? 'mile' : 'km'}`, h('div', { class: 'scroll' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, imperial() ? 'Mile' : 'Km'),
      h('th', {}, isPace ? 'Pace' : 'Speed'), h('th', {}), h('th', { class: 'r' }, 'HR'), h('th', { class: 'r' }, 'Climb'))), tb))));
  }
  // Zones
  if (w.zones?.length) {
    const total = w.zones.reduce((a, z) => a + z.seconds, 0) || 1;
    const colors = ['#94a3b8', '#3b82f6', '#22c55e', '#f59e0b', '#ef4444'];
    grid.append(card('Heart rate zones', ...w.zones.map((z, i) => h('div', { class: 'zone-row' },
      h('span', {}, h('b', {}, 'Zone ' + z.zone), h('div', { class: 'small muted' }, `${z.low}–${z.high} bpm`)),
      h('div', { class: 'bar-bg' }, h('div', { class: 'bar', style: { width: (z.seconds / total * 100) + '%', background: colors[i], height: '14px' } })),
      h('span', { class: 'num r' }, fmtDuration(z.seconds), h('div', { class: 'small muted' }, fmtNum(z.seconds / total * 100) + '%')))),
    h('p', { class: 'small muted' }, `Max heart rate ${fmtNum(w.maxHr)} bpm (${{ setting: 'from Settings', age: '220 − age', observed: 'highest recorded this year' }[w.maxHrFrom] || ''}).`)));
  }
  // Best efforts
  if (w.efforts?.length && isPace) {
    grid.append(card('Best efforts in this workout', ...w.efforts.map(e => h('div', { class: 'record' }, h('span', {}, e.name),
      h('span', { class: 'rv' }, fmtDuration(e.seconds), h('div', { class: 'small muted' }, fmtPace(e.seconds / (e.distance / 1000))))))));
  }
  // Segments and laps
  const laps = (w.events || []).filter(e => e.type === 'lap' || e.type === 'segment');
  if (w.activities?.length > 1) {
    grid.append(card('Segments', h('table', {}, h('tbody', {}, w.activities.map(a => h('tr', {}, h('td', {}, activityName(a.activity_type)),
      h('td', {}, fmtTime(a.start)), h('td', { class: 'r num' }, a.end ? fmtDuration(a.end - a.start) : '')))))));
  }
  if (laps.length) {
    grid.append(card(`Laps and segments (${laps.length})`, h('div', { class: 'scroll', style: { maxHeight: '320px' } }, h('table', {}, h('tbody', {}, laps.map((e, i) => h('tr', {},
      h('td', {}, `${e.type} ${i + 1}`), h('td', {}, fmtDuration(e.start - w.start)), h('td', { class: 'r num' }, fmtDuration(e.end - e.start)))))))));
  }
  // All statistics
  if (w.statistics?.length) {
    grid.append(card('All statistics', h('div', { class: 'scroll' }, h('table', {}, h('thead', {}, h('tr', {}, ...['Type', 'Average', 'Min', 'Max', 'Total'].map((x, i) => h('th', { class: i ? 'r' : '' }, x)))),
      h('tbody', {}, w.statistics.map(st => h('tr', {}, h('td', {}, typeLabel(st.type), h('span', { class: 'muted small' }, ' ' + prettyUnit(st.unit))),
        ...['average', 'minimum', 'maximum', 'sum'].map(k => h('td', { class: 'r num' }, st[k] == null ? '' : fmtCompact(st[k] * scaleFor(st.unit)))))))))));
  }
  if (grid.children.length) main.append(grid);
};

function weatherText(m) {
  const t = m.HKWeatherTemperature.split(' ');
  let v = parseFloat(t[0]);
  if (t[1] === 'degF' && !imperial()) v = (v - 32) * 5 / 9;
  if (t[1] === 'degC' && imperial()) v = v * 9 / 5 + 32;
  const hum = m.HKWeatherHumidity ? ` · ${fmtNum(parseFloat(m.HKWeatherHumidity) / (parseFloat(m.HKWeatherHumidity) > 100 ? 100 : 1))}%` : '';
  return fmtNum(v) + (imperial() ? '°F' : '°C') + hum;
}

function lowerBound(arr, x) { let lo = 0, hi = arr.length; while (lo < hi) { const m = (lo + hi) >> 1; if (arr[m] < x) lo = m + 1; else hi = m; } return lo; }

// smoothSpeed: metres per second from distance over a ±15 s window, which is steadier than the per-fix GPS speed.
function smoothSpeed(route) {
  const out = new Array(route.length).fill(null);
  let a = 0, b = 0;
  for (let i = 0; i < route.length; i++) {
    while (route[a][0] < route[i][0] - 15) a++;
    while (b < route.length - 1 && route[b + 1][0] <= route[i][0] + 15) b++;
    const dt = route[b][0] - route[a][0];
    if (dt > 3) out[i] = (route[b][4] - route[a][4]) / dt;
  }
  return out;
}

function rampColor(f) { // blue → green → yellow → red
  const stops = [[37, 99, 235], [22, 163, 74], [234, 179, 8], [220, 38, 38]];
  const x = f * (stops.length - 1), i = Math.min(stops.length - 2, Math.floor(x)), t = x - i;
  const c = stops[i].map((v, k) => Math.round(v + (stops[i + 1][k] - v) * t));
  return `rgb(${c.join(',')})`;
}

function makeMap(el) {
  const map = L.map(el, { scrollWheelZoom: true, preferCanvas: true });
  const tiles = window.healthStatus?.tiles || {};
  L.tileLayer(tiles.url || 'https://tile.openstreetmap.org/{z}/{x}/{y}.png', {
    maxZoom: 19, attribution: tiles.attribution || '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
  }).addTo(map);
  onCleanup(() => map.remove());
  observeWidth(el, () => map.invalidateSize());
  return map;
}

// ---------- all routes ----------

pages.map = async (main, _, params) => {
  const state = { type: params.get('type') || '', range: params.get('range') || '365' };
  const all = await api('workouts');
  const withRoute = all.filter(w => w.hasRoute);
  const typesCount = {};
  for (const w of withRoute) typesCount[w.activityType] = (typesCount[w.activityType] || 0) + 1;
  const years = [...new Set(withRoute.map(w => new Date(w.start * 1000).getFullYear()))].sort((a, b) => b - a);
  const mapEl = h('div', { class: 'map tall' });
  const info = h('span', { class: 'muted small' });
  const legend = h('div', { class: 'legend' });
  main.replaceChildren(h('h1', {}, 'Map'),
    h('div', { class: 'toolbar' },
      h('select', { onchange: e => { state.type = e.target.value; draw(); } }, h('option', { value: '' }, 'All activities'),
        Object.entries(typesCount).sort((a, b) => b[1] - a[1]).map(([t, n]) => h('option', { value: t, selected: t === state.type }, `${activityName(t)} (${n})`))),
      h('select', { onchange: e => { state.range = e.target.value; draw(); } },
        [['90', 'Last 90 days'], ['365', 'Last 12 months'], ...years.map(y => ['y' + y, String(y)]), ['all', 'All time']].map(([v, l]) => h('option', { value: v, selected: v === state.range }, l))),
      info),
    card(null, mapEl, legend));
  if (!withRoute.length) { mapEl.replaceWith(empty('No workouts with GPS routes yet.')); return; }
  await refreshStatus();
  const map = makeMap(mapEl);
  map.setView([20, 0], 2);
  let layer;
  async function draw() {
    setParams(Object.fromEntries(Object.entries(state).filter(([, v]) => v)));
    const p = {};
    if (state.type) p.type = state.type;
    if (state.range.startsWith('y')) { p.from = state.range.slice(1) + '-01-01'; p.to = state.range.slice(1) + '-12-31'; }
    else if (state.range === 'all') p.from = '2000-01-01';
    else p.from = daysAgo(+state.range);
    info.textContent = 'Loading routes…';
    const lines = await api('routes', p);
    if (layer) layer.remove();
    layer = L.featureGroup().addTo(map);
    const types = {};
    let dist = 0;
    for (const r of lines) {
      types[r.activityType] = (types[r.activityType] || 0) + 1;
      dist += r.distance;
      L.polyline(r.points, { color: activityColor(r.activityType), weight: 3, opacity: lines.length > 50 ? 0.45 : 0.8 })
        .bindTooltip(`${activityName(r.activityType)} · ${fmtDate(r.start)} · ${fmtDist(r.distance)}`, { sticky: true })
        .on('click', () => location.hash = '#/workout/' + r.uuid).addTo(layer);
    }
    info.textContent = `${lines.length} routes · ${fmtDist(dist, 0)}`;
    legend.replaceChildren(...Object.entries(types).map(([t, n]) => h('span', {}, h('i', { style: { background: activityColor(t) } }), `${activityName(t)} (${n})`)));
    if (lines.length) map.fitBounds(layer.getBounds(), { padding: [20, 20] });
  }
  draw();
};

// ---------- sleep ----------

const STAGES = [['deep', 'Deep', '#3730a3'], ['core', 'Core', '#3b82f6'], ['rem', 'REM', '#06b6d4'], ['asleep', 'Asleep (unspecified)', '#8b5cf6'], ['awake', 'Awake', '#f97316']];

pages.sleep = async (main, _, params) => {
  const state = { range: params.get('range') || '30' };
  const body = h('div');
  main.replaceChildren(h('h1', {}, 'Sleep'), h('div', { class: 'toolbar' },
    seg([['14', '2 weeks'], ['30', '30 days'], ['90', '90 days'], ['365', '1 year']], state.range, v => { state.range = v; draw(); })), body);
  async function draw() {
    resetPage();
    setParams({ range: state.range });
    loading(body);
    const nights = await api('sleep', { from: daysAgo(+state.range - 1), to: today() });
    if (!nights.length) { body.replaceChildren(card(null, empty('No sleep data in this period.'))); return; }
    const avg = f => { const xs = nights.map(f).filter(v => v != null && isFinite(v)); return xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : null; };
    // Clock times as minutes from noon, so 23:30 and 00:30 average to midnight.
    const clock = t => { const d = new Date(t * 1000); let m = d.getHours() * 60 + d.getMinutes() - 720; if (m < 0) m += 1440; return m; };
    const fmtClock = m => { if (m == null) return '—'; const x = Math.round(m + 720) % 1440; return `${String(Math.floor(x / 60)).padStart(2, '0')}:${String(x % 60).padStart(2, '0')}`; };
    const beds = nights.filter(n => n.start).map(n => clock(n.start));
    const sd = beds.length > 1 ? Math.sqrt(beds.reduce((a, b) => a + (b - avg(n => n.start ? clock(n.start) : null)) ** 2, 0) / beds.length) : null;
    const stagesChart = h('div', { class: 'chart' }), clockChart = h('div', { class: 'chart' });
    const nightCard = card('Night');
    body.replaceChildren(
      card(null, h('div', { class: 'stats' }, stat('Avg asleep', fmtHM(avg(n => n.asleep))), stat('Avg in bed', fmtHM(avg(n => n.inBed || null))),
        stat('Avg bedtime', fmtClock(avg(n => n.start ? clock(n.start) : null))), stat('Avg wake', fmtClock(avg(n => n.end ? clock(n.end) : null))),
        stat('Bedtime spread', sd == null ? '—' : '±' + fmtNum(sd) + ' min'), stat('Avg deep', fmtHM(avg(n => n.stages.deep))), stat('Avg REM', fmtHM(avg(n => n.stages.rem))),
        stat('Nights ≥ 7 h', fmtNum(nights.filter(n => n.asleep >= 420).length / nights.length * 100) + '%'))),
      card('Sleep stages per night', stagesChart, h('p', { class: 'small muted' }, 'Click a night for its timeline. When several apps record the same night, the one with the most sleep is used.')),
      h('div', { class: 'grid cols-2' }, card('Bedtime and wake time', clockChart), nightCard));
    const xs = nights.map(n => new Date(n.date + 'T00:00').getTime() / 1000);
    const used = STAGES.filter(([k]) => nights.some(n => n.stages[k] > 0));
    const u = stackedBars(stagesChart, { xs, height: 280, stacks: used.map(([k, label, color]) => ({ label, color, values: nights.map(n => (n.stages[k] || 0)) })),
      valueFmt: fmtHM, axisFmt: v => fmtNum(v / 60) + 'h', incrs: MINUTE_INCRS, xFmt: (u, vals) => vals.map(v => fmtDateShort(v)) });
    u.over.addEventListener('click', () => { if (u.cursor.idx != null) showNight(nights[u.cursor.idx].date); });
    u.over.style.cursor = 'pointer';
    chart(clockChart, { xs, height: 240, incrs: [60, 120, 180, 240, 360], yRange: (u, mn, mx) => [Math.min(mn, 480) - 30, Math.max(mx, 1260) + 30], axisFmt: fmtClock, series: [
      { label: 'Bedtime', color: '#8b5cf6', values: nights.map(n => n.start ? clock(n.start) : null), points: true, width: 1, value: (u, v) => fmtClock(v) },
      { label: 'Wake', color: '#f59e0b', values: nights.map(n => n.end ? clock(n.end) : null), points: true, width: 1, value: (u, v) => fmtClock(v) },
    ] });
    async function showNight(date) {
      nightCard.replaceChildren(h('h2', {}, 'Night of ' + new Date(date + 'T12:00').toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' })));
      try {
        const n = await api('sleep', { date });
        nightCard.append(hypnogram(n), h('div', { class: 'stats', style: { marginTop: '12px' } }, stat('Asleep', fmtHM(n.asleep)),
          ...used.map(([k, label]) => stat(label, fmtHM(n.stages[k] || 0)))), h('p', { class: 'small muted' }, 'Source: ' + n.source));
      } catch (e) { nightCard.append(h('p', { class: 'error' }, e.message)); }
    }
    showNight(nights[nights.length - 1].date);
  }
  draw();
};

function hypnogram(n) {
  const segs = n.segments.filter(s => s.stage !== 'inBed');
  if (!segs.length) return empty('No stage data.');
  const t0 = Math.min(...segs.map(s => s.s)), t1 = Math.max(...segs.map(s => s.e));
  const rows = ['awake', 'rem', 'core', 'deep', 'asleep'].filter(k => segs.some(s => s.stage === k));
  const W = 600, rowH = 26, H = rows.length * rowH + 22;
  const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, width: '100%', preserveAspectRatio: 'none', style: 'max-height:200px' });
  const X = t => 70 + (t - t0) / (t1 - t0) * (W - 80);
  rows.forEach((k, i) => svg.append(s('text', { x: 0, y: i * rowH + 17, 'font-size': 11, fill: cssVar('--muted') }, STAGES.find(x => x[0] === k)[1].replace(' (unspecified)', ''))));
  for (const sg of segs) {
    const i = rows.indexOf(sg.stage);
    if (i < 0) continue;
    svg.append(s('rect', { x: X(sg.s), y: i * rowH + 4, width: Math.max(1, X(sg.e) - X(sg.s)), height: rowH - 8, rx: 3, fill: STAGES.find(x => x[0] === sg.stage)[2],
      onmousemove: e => showTip(e, `${STAGES.find(x => x[0] === sg.stage)[1]} ${fmtTime(sg.s)}–${fmtTime(sg.e)}`), onmouseleave: hideTip }));
  }
  for (let t = Math.ceil(t0 / 3600) * 3600; t < t1; t += 3600) {
    svg.append(s('text', { x: X(t), y: H - 4, 'font-size': 10, fill: cssVar('--muted'), 'text-anchor': 'middle' }, fmtTime(t).replace(/:00/, '')));
  }
  return svg;
}

// ---------- records ----------

pages.records = async (main) => {
  const [rec, eff] = await Promise.all([api('records'), api('records/efforts', { type: 'running' }).catch(() => [])]);
  const grid = h('div', { class: 'grid cols-3' });
  main.replaceChildren(h('h1', {}, 'Personal records'), grid);
  const fmtRec = r => {
    switch (r.unit) {
      case 's': return fmtDuration(r.value);
      case 'm': return r.name === 'Most climbing' ? fmtElev(r.value) : fmtDist(r.value);
      case 's/km': return paceSports.has(r.type) ? fmtPace(r.value) : fmtSpeed(1000 / r.value);
      case 'kcal': return fmtKcal(r.value);
      default: return fmtNum(r.value * scaleFor(r.unit), typeDec(r.type)) + ' ' + prettyUnit(r.unit);
    }
  };
  const recRow = r => h('div', { class: 'record' }, h('span', {}, r.unit === 's/km' && !paceSports.has(r.type) ? 'Fastest average speed' : r.name, h('div', { class: 'small muted' },
    r.uuid ? h('a', { href: '#/workout/' + r.uuid }, fmtDate(r.when)) : fmtDate(r.when))), h('span', { class: 'rv' }, fmtRec(r)));

  if (eff.length) grid.append(card('Fastest running efforts', ...eff.map(e => h('div', { class: 'record' }, h('span', {}, e.name,
    h('div', { class: 'small muted' }, h('a', { href: '#/workout/' + e.uuid }, fmtDate(e.start)))),
    h('span', { class: 'rv' }, fmtDuration(e.seconds), h('div', { class: 'small muted' }, fmtPace(e.seconds / (e.distance / 1000))))))));
  const days = rec.records.filter(r => r.group === 'day'), health = rec.records.filter(r => r.group === 'health');
  if (days.length) grid.append(card('Best days', ...days.map(recRow)));
  if (health.length) grid.append(card('Health bests', ...health.map(recRow)));
  if (rec.rings?.some(r => r.daysClosed)) grid.append(card('Ring streaks', ...rec.rings.map(r => h('div', { class: 'record' }, h('span', {}, r.ring,
    h('div', { class: 'small muted' }, `${fmtNum(r.daysClosed)} days closed · current streak ${r.current}`)), h('span', { class: 'rv' }, r.longest + ' days',
    h('div', { class: 'small muted' }, r.longest ? 'ended ' + fmtDate(new Date(r.longestEnded + 'T12:00') / 1000) : ''))))));
  const byType = {};
  for (const r of rec.records.filter(r => r.group === 'workout')) (byType[r.type] ||= []).push(r);
  for (const t of (rec.totals || []).map(t => t.activityType)) {
    const tot = rec.totals.find(x => x.activityType === t);
    if (!byType[t]) continue;
    grid.append(card(null, h('h2', { style: { color: activityColor(t) } }, activityName(t)),
      h('p', { class: 'small muted' }, `${fmtNum(tot.count)} workouts since ${fmtDate(tot.first)} · ${fmtHM(tot.duration / 60)}${tot.distance ? ' · ' + fmtDist(tot.distance, 0) : ''}`),
      ...byType[t].map(recRow)));
  }
  if (!grid.children.length) grid.append(card(null, empty('No records yet.')));
};

// ---------- settings ----------

pages.settings = async (main) => {
  const profile = await api('profile').catch(() => ({}));
  const maxIn = h('input', { type: 'number', min: 120, max: 230, placeholder: String(profile.maxHr || ''), value: settings.maxHr, style: { width: '110px' } });
  const sexNames = { 1: 'female', 2: 'male', 3: 'other' };
  main.replaceChildren(h('h1', {}, 'Settings'),
    card('Display',
      h('div', { class: 'toolbar' }, h('span', { style: { width: '160px' } }, 'Units'),
        seg([['metric', 'Kilometres'], ['imperial', 'Miles']], settings.units, v => { settings.units = v; saveSettings(); })),
      h('div', { class: 'toolbar' }, h('span', { style: { width: '160px' } }, 'Theme'),
        seg([['auto', 'System'], ['light', 'Light'], ['dark', 'Dark']], settings.theme, v => { settings.theme = v; saveSettings(); applyTheme(); }))),
    card('Heart rate',
      h('div', { class: 'toolbar' }, h('span', { style: { width: '160px' } }, 'Maximum heart rate'), maxIn,
        h('button', { onclick: () => { settings.maxHr = maxIn.value; saveSettings(); apiCache.clear(); maxIn.blur(); } }, 'Save'),
        h('span', { class: 'muted small' }, profile.maxHr ? `Without a value: ${profile.maxHr} bpm (${profile.maxHrFrom === 'age' ? '220 − age' : 'highest recorded'})` : '')),
      h('p', { class: 'small muted' }, 'Used for heart rate zones and training load.',
        profile.restingHr ? ` Resting heart rate, 30-day average: ${profile.restingHr} bpm.` : '')),
    card('Profile', h('div', { class: 'stats' }, stat('Age', profile.age ?? '—'), stat('Sex', sexNames[profile.characteristics?.biologicalSex] || '—'))),
    card('About', h('p', { class: 'small muted' }, 'Settings are kept in this browser. Data comes from your MariaDB database, read-only. ',
      h('a', { href: 'logout' }, 'Sign out'))));
};

// ---------- start ----------

refreshStatus();
setInterval(refreshStatus, 60000);
render();
