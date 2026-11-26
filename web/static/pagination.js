"use strict";
const $ = (id) => document.getElementById(id);
const COLORS = { offset: "--c-offset", deferred: "--c-deferred", keyset: "--c-keyset", checkpoint: "--c-checkpoint", nearest: "--c-nearest" };
const color = (k) => getComputedStyle(document.documentElement).getPropertyValue(COLORS[k]).trim();
const fmt = (n) => n.toLocaleString("en-US");
const fmtMs = (ms) => (ms < 1 ? ms.toFixed(2) : ms < 100 ? ms.toFixed(1) : Math.round(ms).toString()) + " ms";
let TOTAL_PAGES = 500000; // replaced by /api/status on load
let strategies = [];

function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "style") e.style.cssText = v;
    else e.setAttribute(k, v);
  }
  for (const k of kids) e.append(k instanceof Node ? k : document.createTextNode(String(k)));
  return e;
}

async function api(path) {
  const r = await fetch(path);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || `HTTP ${r.status}`);
  return j;
}

// ---------- status ----------
async function pollStatus() {
  try {
    const s = await api("/api/status");
    if (s.totalPages && s.totalPages !== TOTAL_PAGES) {
      TOTAL_PAGES = s.totalPages;
      $("c-rows").textContent = fmt(s.totalRows);
      $("c-pages").textContent = fmt(s.totalPages);
      $("of-pages").textContent = "of " + fmt(s.totalPages);
      $("page").max = s.totalPages;
      renderQuick();
    }
    $("status").textContent = s.ready ? "● live" : "◌ " + s.state;
    $("status").style.color = s.ready ? color("keyset") : "";
    if (!s.ready) return setTimeout(pollStatus, 3000);
    $("run").disabled = false;
  } catch {
    $("status").textContent = "offline";
    setTimeout(pollStatus, 5000);
  }
}

// ---------- quick picks ----------
const quickPicks = () => [
  ["1", 1], ["50", 50], ["5,000", 5000], ["50,000", 50000],
  [fmt(TOTAL_PAGES / 2), TOTAL_PAGES / 2], [fmt(TOTAL_PAGES - 1000), TOTAL_PAGES - 1000], [`Last (${fmt(TOTAL_PAGES)})`, TOTAL_PAGES],
];
function renderQuick() {
  const q = $("quick");
  q.replaceChildren(...quickPicks().map(([label, p]) => {
    const b = el("button", { type: "button" }, label);
    b.dataset.page = p;
    b.onclick = () => { $("page").value = p; runAll(); };
    return b;
  }));
}
function markQuick(page) {
  for (const b of $("quick").children) b.classList.toggle("on", Number(b.dataset.page) === page);
}

// ---------- run ----------
let running = false;
async function runAll() {
  if (running) return;
  const page = Math.round(Number($("page").value));
  $("err").hidden = true;
  if (!(page >= 1 && page <= TOTAL_PAGES)) {
    $("err").textContent = `Pick a page between 1 and ${fmt(TOTAL_PAGES)}.`;
    $("err").hidden = false;
    return;
  }
  running = true;
  $("run").disabled = true;
  markQuick(page);
  history.replaceState(null, "", `?page=${page}`);

  const cards = new Map();
  $("results").replaceChildren(...strategies.map((s) => {
    const c = resultCard(s);
    cards.set(s.key, c);
    return c.root;
  }));

  const results = [];
  // One at a time on purpose: concurrent runs would skew each other's timings.
  for (const s of strategies) {
    const c = cards.get(s.key);
    c.ms.textContent = "running…";
    try {
      const r = await api(`/api/pagination/run?strategy=${s.key}&page=${page}`);
      results.push(r);
      fillCard(c, r);
    } catch (e) {
      c.ms.textContent = "error";
      $("err").textContent = e.message;
      $("err").hidden = false;
      if (String(e.message).startsWith("Easy there")) break;
    }
  }
  scaleBars(cards, results);
  loadRows(page);
  running = false;
  $("run").disabled = false;
}

function resultCard(s) {
  const bar = el("div", { class: "bar", style: `background:${color(s.key)}` });
  const ms = el("div", { class: "ms" }, "…");
  const meta = el("div", { class: "res-meta" });
  const sql = el("pre");
  const plan = el("pre");
  const details = el("details", {}, el("summary", {}, "SQL and query plan"),
    el("div", { class: "cols" }, el("div", {}, sql), el("div", {}, plan)));
  const badge = el("span", { class: "badge", hidden: "" });
  const root = el("div", { class: "res" },
    el("div", { class: "res-head" },
      el("div", { class: "res-name" }, el("span", { class: "dot", style: `background:${color(s.key)}` }), s.name, badge),
      el("div", { class: "bar-track" }, bar),
      ms),
    meta, details);
  return { root, bar, ms, meta, sql, plan, badge, details };
}

function fillCard(c, r) {
  c.sql.textContent = r.sql;
  if (r.timedOut) {
    c.ms.textContent = "> 5 s";
    c.meta.replaceChildren(el("span", {}, "Cancelled by the 5s statement timeout. That's the point."));
    c.plan.textContent = "(timed out before finishing)";
    return;
  }
  c.ms.textContent = fmtMs(r.executionMs);
  c.plan.textContent = r.plan;
  const kb = r.buffers * 8;
  c.meta.replaceChildren(
    el("span", {}, "rows touched ", el("b", {}, fmt(r.rowsTouched))),
    el("span", {}, "buffers ", el("b", {}, fmt(r.buffers)), ` (${kb >= 1024 ? (kb / 1024).toFixed(1) + " MB" : kb + " KB"})`),
    el("span", {}, "planning ", el("b", {}, fmtMs(r.planningMs))),
    ...(r.cached ? [el("span", {}, "cached result")] : []),
  );
}

function scaleBars(cards, results) {
  const ok = results.filter((r) => !r.timedOut);
  if (!ok.length) return;
  const min = 0.01, max = Math.max(...ok.map((r) => r.executionMs), 1);
  const best = ok.reduce((a, b) => (b.executionMs < a.executionMs ? b : a));
  const worst = ok.reduce((a, b) => (b.executionMs > a.executionMs ? b : a));
  for (const r of results) {
    const c = cards.get(r.strategy);
    const w = r.timedOut ? 100 : Math.max(2, (Math.log10(Math.max(r.executionMs, min) / min) / Math.log10(max / min)) * 100);
    requestAnimationFrame(() => (c.bar.style.width = w + "%"));
    if (r === best && ok.length > 1) {
      c.badge.textContent = "fastest";
      c.badge.className = "badge win";
      c.badge.hidden = false;
    } else if ((r === worst && worst.executionMs > best.executionMs * 5) || r.timedOut) {
      const x = Math.round(r.executionMs / best.executionMs);
      c.badge.textContent = r.timedOut ? "timed out" : `${fmt(x)}× slower`;
      c.badge.className = "badge slow";
      c.badge.hidden = false;
    }
  }
}

async function loadRows(page) {
  try {
    const rows = await api(`/api/pagination/rows?page=${page}`);
    $("rows").replaceChildren(...rows.map((r) =>
      el("tr", {}, el("td", {}, r.id), el("td", {}, r.createdAt.replace("T", " ").replace("Z", "")), el("td", {}, r.title))));
    $("rows-wrap").hidden = false;
  } catch {}
}

// ---------- sweep chart ----------
async function loadSweep() {
  let sw;
  try {
    sw = await api("/api/pagination/sweep");
  } catch {
    $("sweep-sub").textContent = "Measuring on the server… check back in a minute.";
    return setTimeout(loadSweep, 15000);
  }
  const when = new Date(sw.measuredAt);
  $("sweep-sub").textContent = `Measured on this server ${when.toLocaleString()} (median of 3 runs per point, log scale).`;
  drawChart(sw.points);
}

function drawChart(points) {
  const W = 960, H = 380, L = 64, R = 16, T = 16, B = 46;
  const xs = points.map((p) => p.page);
  const all = points.flatMap((p) => Object.values(p.ms)).filter((v) => v > 0);
  const timedOut = points.some((p) => Object.values(p.ms).some((v) => v < 0));
  const yMin = Math.pow(10, Math.floor(Math.log10(Math.min(...all)))), yMax = Math.pow(10, Math.ceil(Math.log10(Math.max(...all))));
  const x = (i) => L + (i / (points.length - 1)) * (W - L - R);
  const y = (v) => T + (1 - (Math.log10(v) - Math.log10(yMin)) / (Math.log10(yMax) - Math.log10(yMin))) * (H - T - B);
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "Query time by page depth for each pagination strategy");
  const add = (tag, attrs, text) => {
    const n = document.createElementNS(ns, tag);
    for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
    if (text != null) n.textContent = text;
    svg.append(n);
    return n;
  };
  const muted = getComputedStyle(document.documentElement).getPropertyValue("--muted").trim();
  const line = getComputedStyle(document.documentElement).getPropertyValue("--line").trim();
  for (let d = yMin; d <= yMax; d *= 10) {
    add("line", { x1: L, x2: W - R, y1: y(d), y2: y(d), stroke: line });
    add("text", { x: L - 8, y: y(d) + 4, "text-anchor": "end", "font-size": 12, fill: muted, "font-family": "monospace" }, d >= 1 ? `${d} ms` : `${d} ms`);
  }
  points.forEach((p, i) => add("text", { x: x(i), y: H - B + 20, "text-anchor": "middle", "font-size": 12, fill: muted, "font-family": "monospace" }, "p." + fmt(p.page)));
  add("text", { x: (L + W - R) / 2, y: H - 6, "text-anchor": "middle", "font-size": 12, fill: muted }, "page number (20 rows per page)");
  for (const s of strategies) {
    // Timed-out points (-1) are pinned to the top edge and drawn hollow.
    const pts = points.map((p, i) => [x(i), p.ms[s.key] < 0 ? T : y(Math.max(p.ms[s.key], yMin)), p.ms[s.key] < 0]);
    add("polyline", { points: pts.map((q) => q[0] + "," + q[1]).join(" "), fill: "none", stroke: color(s.key), "stroke-width": 2.5, "stroke-linejoin": "round" });
    pts.forEach(([cx, cy, to], i) => {
      const c = add("circle", to ? { cx, cy, r: 5, fill: "none", stroke: color(s.key), "stroke-width": 2 } : { cx, cy, r: 4, fill: color(s.key) });
      const t = document.createElementNS(ns, "title");
      t.textContent = `${s.name} · page ${fmt(points[i].page)}: ${to ? "timed out (> 20 s)" : fmtMs(points[i].ms[s.key])}`;
      c.append(t);
    });
  }
  $("chart").replaceChildren(svg);
  $("legend").replaceChildren(...strategies.map((s) => el("span", {}, el("span", { class: "dot", style: `background:${color(s.key)}` }), s.name)),
    ...(timedOut ? [el("span", {}, "○ hollow at the top = timed out after 20 s")] : []));
}

function renderExplain() {
  $("explain").replaceChildren(...strategies.map((s) =>
    el("div", {}, el("span", { class: "dot", style: `background:${color(s.key)}` }), el("p", {}, el("strong", {}, s.name + ". "), s.blurb))));
}

// ---------- boot ----------
(async function boot() {
  $("run").disabled = true;
  $("run").onclick = runAll;
  $("page").addEventListener("keydown", (e) => e.key === "Enter" && runAll());
  const qp = Number(new URLSearchParams(location.search).get("page"));
  if (qp >= 1 && qp <= TOTAL_PAGES) $("page").value = qp;
  renderQuick();
  strategies = await api("/api/pagination/strategies");
  renderExplain();
  await pollStatus();
  loadSweep();
})();
