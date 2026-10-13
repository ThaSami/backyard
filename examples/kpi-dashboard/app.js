// KPI Dashboard: a warehouse named query → stat tiles, an SVG trend chart
// (with crosshair tooltip) and a data table. When the warehouse is not
// available (not configured, Cloudflare edition, query missing, no access) it
// falls back to clearly labeled demo data.
//
// Expected query result: one row per day — a date column, then numeric
// columns, e.g. {day: "2026-01-01", orders: 231, returns: 22}.

const params = new URLSearchParams(location.search);
const QUERY = params.get("query") || "kpi_daily";
const MAX_SERIES = 4; // more series than this go to the table only

const $ = (sel) => document.querySelector(sel);
const state = { days: 30, data: null, hover: null };

// ---------------------------------------------------------------------------
// Data

function humanize(name) {
  const s = String(name).replace(/[_-]+/g, " ").trim();
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function toDataset(result) {
  const cols = result.columns ?? [];
  const rows = result.rows ?? [];
  const xcol =
    cols.find((c) => /date|time/i.test(c.type)) ?? cols.find((c) => /^(day|date|week|month|ts|time)$/i.test(c.name)) ?? cols[0];
  if (!xcol) return { x: [], series: [], columns: cols, rows };
  const numeric = cols.filter(
    (c) => c !== xcol && rows.length && rows.filter((r) => r[c.name] !== null && Number.isFinite(Number(r[c.name]))).length >= rows.length / 2,
  );
  const sorted = [...rows].sort((a, b) => String(a[xcol.name]).localeCompare(String(b[xcol.name])));
  return {
    x: sorted.map((r) => String(r[xcol.name]).slice(0, 10)),
    series: numeric.map((c) => ({
      key: c.name,
      label: humanize(c.name),
      values: sorted.map((r) => (r[c.name] === null || r[c.name] === undefined ? null : Number(r[c.name]))),
    })),
    columns: cols,
    rows: sorted,
  };
}

// Deterministic demo data (same numbers on every reload).
function demoResult(days) {
  let seed = 42;
  const rand = () => ((seed = (seed * 1664525 + 1013904223) % 4294967296) / 4294967296);
  const rows = [];
  const today = new Date();
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(today.getFullYear(), today.getMonth(), today.getDate() - i);
    const weekend = d.getDay() === 0 || d.getDay() === 6;
    const orders = Math.round((weekend ? 150 : 240) + (days - i) * 0.9 + 25 * Math.sin(i / 4) + rand() * 30);
    rows.push({ day: d.toLocaleDateString("en-CA"), orders, returns: Math.round(orders * (0.07 + rand() * 0.05)) });
  }
  return { columns: [{ name: "day", type: "DATE" }, { name: "orders", type: "INT64" }, { name: "returns", type: "INT64" }], rows };
}

function explain(err) {
  if (!window.backyard) return "This page is not served by Backyard (open it with backyard dev or deploy it), so it shows demo data.";
  switch (err.code) {
    case "unsupported":
      return "The data warehouse is not configured on this Backyard server, so this page shows demo data.";
    case "not_found":
      return `There is no named query “${QUERY}” on this server, so this page shows demo data.`;
    case "forbidden":
      return `You are not allowed to run “${QUERY}”, so this page shows demo data.`;
    default:
      return `The warehouse query failed (${err.message}), so this page shows demo data.`;
  }
}

async function load(days) {
  try {
    const { queries } = await backyard.warehouse.sources();
    const q = queries.find((x) => x.name === QUERY);
    if (!q) throw Object.assign(new Error("no such query"), { code: "not_found" });
    const result = await backyard.warehouse.named(QUERY, (q.params ?? []).includes("days") ? { days } : {});
    return { ...toDataset(result), demo: null, truncated: result.truncated, description: q.description };
  } catch (err) {
    return { ...toDataset(demoResult(days)), demo: explain(err) };
  }
}

// ---------------------------------------------------------------------------
// Formatting

const compact = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });
const full = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 });
const parseDay = (s) => {
  const [y, m, d] = s.split("-").map(Number);
  return Number.isFinite(y) && m && d ? new Date(y, m - 1, d) : null;
};
const shortDate = (s) => parseDay(s)?.toLocaleDateString(undefined, { month: "short", day: "numeric" }) ?? s;
const longDate = (s) => parseDay(s)?.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric", year: "numeric" }) ?? s;
const sum = (vals) => vals.reduce((a, v) => a + (v ?? 0), 0);

// ---------------------------------------------------------------------------
// Tiles

function renderTiles(data) {
  const tiles = data.series.slice(0, 3).map((s, i) => {
    const vals = s.values;
    const n = vals.length;
    const win = n >= 14 ? 7 : Math.floor(n / 2);
    const recent = sum(vals.slice(n - win));
    const prior = sum(vals.slice(n - 2 * win, n - win));
    const el = document.createElement("article");
    el.className = "tile" + (i === 0 ? " hero" : "");
    el.innerHTML = `<p class="label"></p><p class="value"></p><p class="delta"></p>`;
    el.querySelector(".label").textContent = `${s.label} · last ${n} days`;
    el.querySelector(".value").textContent = compact.format(sum(vals));
    const delta = el.querySelector(".delta");
    if (win > 0 && prior > 0) {
      const pct = ((recent - prior) / prior) * 100;
      delta.textContent = `${pct >= 0 ? "▲" : "▼"} ${Math.abs(pct).toFixed(1)}% · last ${win} days vs the ${win} before`;
    } else {
      delta.textContent = `${full.format(sum(vals) / Math.max(1, n))} per day on average`;
    }
    return el;
  });
  $("#tiles").replaceChildren(...tiles);
}

// ---------------------------------------------------------------------------
// Chart (inline SVG): 2px lines, light area wash for a single series,
// hairline grid, end labels when they do not collide, crosshair tooltip.

const NS = "http://www.w3.org/2000/svg";
const svgEl = (name, attrs = {}) => {
  const el = document.createElementNS(NS, name);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  return el;
};

function niceMax(max) {
  if (!(max > 0)) return { top: 1, step: 0.25 };
  const raw = max / 4;
  const pow = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * pow).find((s) => s >= raw);
  return { top: step * Math.ceil(max / step), step };
}

let geometry = null;

function renderChart(data) {
  const svg = $("#svg");
  const series = data.series.slice(0, MAX_SERIES);
  const n = data.x.length;
  $("#empty").hidden = n > 0 && series.length > 0;
  svg.replaceChildren();
  geometry = null;
  if (!n || !series.length) return;

  const width = Math.max(320, $("#chart").clientWidth);
  const height = width < 560 ? 240 : 320;
  const m = { l: 44, r: series.length > 1 ? 64 : 56, t: 12, b: 28 };
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("width", width);
  svg.setAttribute("height", height);

  const max = Math.max(0, ...series.flatMap((s) => s.values.filter((v) => v !== null)));
  const { top, step } = niceMax(max);
  const x = (i) => m.l + (n === 1 ? (width - m.l - m.r) / 2 : (i / (n - 1)) * (width - m.l - m.r));
  const y = (v) => m.t + (1 - v / top) * (height - m.t - m.b);

  // Grid + y ticks
  for (let v = 0; v <= top + 1e-9; v += step) {
    svg.append(svgEl("line", { x1: m.l, x2: width - m.r, y1: y(v), y2: y(v), class: v === 0 ? "baseline" : "grid" }));
    const t = svgEl("text", { x: m.l - 8, y: y(v) + 4, "text-anchor": "end", class: "tick" });
    t.textContent = compact.format(v);
    svg.append(t);
  }
  // x ticks: about one per 80px, at most six
  const every = Math.max(1, Math.ceil(n / Math.max(2, Math.min(6, Math.floor((width - m.l - m.r) / 80)))));
  for (let i = 0; i < n; i += every) {
    const t = svgEl("text", { x: x(i), y: height - 8, "text-anchor": "middle", class: "tick" });
    t.textContent = shortDate(data.x[i]);
    svg.append(t);
  }

  // Lines (gaps for null values)
  series.forEach((s, si) => {
    let d = "";
    let pen = false;
    s.values.forEach((v, i) => {
      if (v === null) {
        pen = false;
        return;
      }
      d += `${pen ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`;
      pen = true;
    });
    if (series.length === 1 && s.values.every((v) => v !== null)) {
      svg.append(svgEl("path", { d: `${d}L${x(n - 1)},${y(0)}L${x(0)},${y(0)}Z`, class: "area", style: `--c: var(--s${si + 1})` }));
    }
    svg.append(svgEl("path", { d, class: "line", style: `--c: var(--s${si + 1})` }));
  });

  // End labels unless they would collide (then the legend + tooltip carry identity).
  const ends = series.map((s, si) => {
    const i = s.values.findLastIndex((v) => v !== null);
    return { si, i, v: s.values[i], py: i >= 0 ? y(s.values[i]) : null };
  });
  const collide = ends.some((a, i) => ends.some((b, j) => i < j && a.py !== null && b.py !== null && Math.abs(a.py - b.py) < 16));
  for (const e of ends) {
    if (e.i < 0) continue;
    svg.append(svgEl("circle", { cx: x(e.i), cy: e.py, r: 4, class: "end", style: `--c: var(--s${e.si + 1})` }));
    if (!collide) {
      const t = svgEl("text", { x: x(e.i) + 8, y: e.py + 4, class: "end-label" });
      t.textContent = compact.format(e.v);
      svg.append(t);
    }
  }

  // Hover layer
  const cross = svgEl("line", { y1: m.t, y2: height - m.b, class: "cross", visibility: "hidden" });
  const dots = series.map((_, si) => svgEl("circle", { r: 4, class: "dot", style: `--c: var(--s${si + 1})`, visibility: "hidden" }));
  svg.append(cross, ...dots);
  geometry = { x, y, n, cross, dots, series, width, height };
  if (state.hover !== null) showHover(Math.min(state.hover, n - 1));
}

function showHover(i) {
  if (!geometry) return;
  const { x, y, cross, dots, series, width, height } = geometry;
  state.hover = i;
  cross.setAttribute("x1", x(i));
  cross.setAttribute("x2", x(i));
  cross.setAttribute("visibility", "visible");
  series.forEach((s, si) => {
    const v = s.values[i];
    dots[si].setAttribute("visibility", v === null ? "hidden" : "visible");
    if (v !== null) {
      dots[si].setAttribute("cx", x(i));
      dots[si].setAttribute("cy", y(v));
    }
  });
  // Tooltip: values lead, labels follow; every series at this x.
  const tip = $("#tip");
  const rows = series.map((s, si) => {
    const row = document.createElement("div");
    row.className = "tip-row";
    row.innerHTML = `<span class="key" style="--c: var(--s${si + 1})"></span><b></b><span></span>`;
    row.querySelector("b").textContent = s.values[i] === null ? "–" : full.format(s.values[i]);
    row.querySelector("span:last-child").textContent = s.label;
    return row;
  });
  const head = Object.assign(document.createElement("div"), { className: "tip-head", textContent: longDate(state.data.x[i]) });
  tip.replaceChildren(head, ...rows);
  tip.hidden = false;
  const left = (x(i) / width) * 100;
  tip.style.left = `${left}%`;
  tip.style.transform = `translate(${left > 60 ? "calc(-100% - 12px)" : "12px"}, 0)`;
  tip.style.top = `${(12 / height) * 100}%`;
}

function hideHover() {
  if (!geometry) return;
  state.hover = null;
  geometry.cross.setAttribute("visibility", "hidden");
  geometry.dots.forEach((d) => d.setAttribute("visibility", "hidden"));
  $("#tip").hidden = true;
}

function indexAt(clientX) {
  const svg = $("#svg");
  const box = svg.getBoundingClientRect();
  const px = ((clientX - box.left) / box.width) * geometry.width;
  let best = 0;
  for (let i = 1; i < geometry.n; i++) if (Math.abs(geometry.x(i) - px) < Math.abs(geometry.x(best) - px)) best = i;
  return best;
}

$("#svg").addEventListener("pointermove", (e) => geometry && showHover(indexAt(e.clientX)));
$("#svg").addEventListener("pointerleave", hideHover);
$("#svg").addEventListener("blur", hideHover);
$("#svg").addEventListener("keydown", (e) => {
  if (!geometry) return;
  const cur = state.hover ?? geometry.n - 1;
  if (e.key === "ArrowLeft") showHover(Math.max(0, cur - 1));
  else if (e.key === "ArrowRight") showHover(Math.min(geometry.n - 1, cur + 1));
  else if (e.key === "Escape") hideHover();
  else return;
  e.preventDefault();
});

// ---------------------------------------------------------------------------
// Legend and table

function renderLegend(data) {
  const series = data.series.slice(0, MAX_SERIES);
  // One series: the chart title names it, no legend box.
  $("#chart-title").textContent = series.length === 1 ? `${series[0].label} per day` : "Daily trend";
  $("#legend").replaceChildren(
    ...(series.length > 1
      ? series.map((s, si) => {
          const item = document.createElement("span");
          item.className = "legend-item";
          item.innerHTML = `<span class="key" style="--c: var(--s${si + 1})"></span><span></span>`;
          item.lastChild.textContent = s.label;
          return item;
        })
      : []),
  );
  if (data.series.length > MAX_SERIES) {
    $("#legend").append(Object.assign(document.createElement("span"), { className: "muted", textContent: `+${data.series.length - MAX_SERIES} more in the table` }));
  }
}

function renderTable(data) {
  const table = $("#table");
  const head = document.createElement("tr");
  for (const c of data.columns) head.append(Object.assign(document.createElement("th"), { textContent: humanize(c.name), scope: "col" }));
  const body = data.rows.map((r) => {
    const tr = document.createElement("tr");
    for (const c of data.columns) {
      const v = r[c.name];
      const td = document.createElement("td");
      td.textContent = typeof v === "number" ? full.format(v) : String(v ?? "");
      if (typeof v === "number") td.className = "num";
      tr.append(td);
    }
    return tr;
  });
  table.replaceChildren(Object.assign(document.createElement("thead"), {}), document.createElement("tbody"));
  table.tHead.append(head);
  table.tBodies[0].append(...body);
}

// ---------------------------------------------------------------------------
// Startup

function render() {
  const data = state.data;
  $("#notice").hidden = !data.demo;
  $("#notice-text").textContent = data.demo ?? "";
  $("#source").textContent = data.demo ? "Demo data" : `Warehouse · ${QUERY}${data.truncated ? " (truncated)" : ""}`;
  $("#source").classList.toggle("demo", Boolean(data.demo));
  renderTiles(data);
  renderLegend(data);
  renderChart(data);
  renderTable(data);
}

let generation = 0;
async function refresh() {
  const mine = ++generation; // only the latest request may render (range clicks can overlap)
  $("#main").classList.add("loading"); // keep the old frame visible while reloading
  try {
    const data = await load(state.days);
    if (mine !== generation) return;
    state.data = data;
    render();
  } finally {
    if (mine === generation) $("#main").classList.remove("loading");
  }
}

document.querySelectorAll("[data-days]").forEach((b) =>
  b.addEventListener("click", () => {
    state.days = Number(b.dataset.days);
    document.querySelectorAll("[data-days]").forEach((x) => x.classList.toggle("active", x === b));
    refresh();
  }),
);

let resizeTimer;
new ResizeObserver(() => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => state.data && renderChart(state.data), 100);
}).observe($("#chart"));

refresh().catch((err) => {
  $("#notice").hidden = false;
  $("#notice-text").textContent = window.backyard ? `Could not load: ${err.message}` : "Open this page through Backyard (backyard dev or a deployed site).";
});
