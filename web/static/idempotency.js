"use strict";
const $ = (id) => document.getElementById(id);
const css = (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();
let meta = { designs: [], scenarios: [] };
let selected = "double_click";
const score = {}; // score[scenario][design] = pass boolean

function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "style") e.style.cssText = v;
    else e.setAttribute(k, v);
  }
  for (const k of kids) if (k != null) e.append(k instanceof Node ? k : document.createTextNode(String(k)));
  return e;
}

async function api(path, opts) {
  const r = await fetch(path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || `HTTP ${r.status}`);
  return j;
}

const designName = (k) => meta.designs.find((d) => d.key === k)?.name ?? k;
const scenario = (k) => meta.scenarios.find((s) => s.key === k);
const dollars = (c) => "$" + (c / 100).toFixed(2);
const codeClass = (s) => (s >= 500 ? "c5xx" : s >= 400 ? "c4xx" : "c2xx");

function renderScenarios() {
  $("scenarios").replaceChildren(...meta.scenarios.map((s) => {
    const b = el("button", { type: "button", class: "scn" + (s.key === selected ? " on" : "") }, el("b", {}, s.name), el("span", {}, s.blurb));
    b.onclick = () => {
      selected = s.key;
      renderScenarios();
    };
    return b;
  }));
}

function renderResults(run) {
  const sc = scenario(run.scenario);
  $("results-panel").hidden = false;
  $("results-title").textContent = `2. What happened: ${sc.name}`;
  $("results-expected").textContent = `Correct outcome: ${sc.expected}`;
  const maxMs = Math.max(...run.results.flatMap((r) => r.attempts.map((a) => a.endMs)), 1);
  $("results").replaceChildren(...run.results.map((r) => {
    const total = r.charges.reduce((a, c) => a + c.amount, 0);
    const rows = r.attempts.flatMap((a) => {
      const left = (a.startMs / maxMs) * 100;
      const width = Math.max(((a.endMs - a.startMs) / maxMs) * 100, 0.8);
      const color = a.lost ? css("--muted") : css(a.status >= 500 ? "--c-offset" : a.status >= 400 ? "--c-deferred" : "--c-keyset");
      return [
        el("div", { class: "tl-row" },
          el("span", {}, "#" + a.attempt),
          el("div", { class: "tl-track", title: `${a.startMs} → ${a.endMs} ms` },
            el("div", { class: "tl-bar" + (a.lost ? " lost" : ""), style: `left:${left}%;width:${width}%;background:${color}` })),
          el("span", { class: "tl-code " + (a.lost ? "" : codeClass(a.status)) }, a.lost ? "lost" : a.status)),
        el("div", { class: "tl-note" }, (a.amount !== 2500 ? `[${dollars(a.amount)} order] ` : "") + a.note),
      ];
    });
    return el("div", { class: "dcard " + (r.pass ? "pass" : "fail") },
      el("div", { class: "dhead" }, el("h3", {}, designName(r.design)), el("span", { class: "big " + (r.pass ? "pass" : "fail") }, r.pass ? "✓ correct" : "✗ wrong")),
      el("p", { class: "verdict" }, r.verdict),
      el("div", { class: "money" }, "money moved: ", el("b", {}, dollars(total)), ` in ${r.charges.length} charge${r.charges.length === 1 ? "" : "s"}`,
        r.charges.length ? ` (#${r.charges.map((c) => c.id).join(", #")})` : ""),
      el("div", { class: "tl" }, ...rows));
  }));
  score[run.scenario] = Object.fromEntries(run.results.map((r) => [r.design, r.pass]));
  renderScore();
}

function renderScore() {
  const head = el("tr", {}, el("th", {}, "Scenario"), ...meta.designs.map((d) => el("th", {}, d.name)));
  const rows = meta.scenarios.map((s) => el("tr", {}, el("td", {}, s.name), ...meta.designs.map((d) => {
    const v = score[s.key]?.[d.key];
    return el("td", { class: v === undefined ? "" : v ? "p" : "f" }, v === undefined ? "·" : v ? "✓" : "✗");
  })));
  const passed = (d) => meta.scenarios.filter((s) => score[s.key]?.[d.key]).length;
  const ran = meta.scenarios.filter((s) => score[s.key]).length;
  const foot = el("tr", {}, el("td", {}, el("b", {}, "Passed")), ...meta.designs.map((d) => el("td", {}, el("b", {}, ran ? `${passed(d)} / ${ran}` : "·"))));
  $("score").replaceChildren(head, ...rows, foot);
}

function renderDocs() {
  $("docs").replaceChildren(...meta.designs.map((d) =>
    el("div", {}, el("h3", {}, d.name), el("p", {}, d.blurb), el("pre", {}, d.code))));
}

let busy = false;
async function run(key) {
  const r = await api(`/api/idempotency/run?scenario=${key}`, { method: "POST" });
  renderResults(r);
}

function setBusy(b) {
  busy = b;
  $("run").disabled = b;
  $("run-all").disabled = b;
}

async function guarded(fn) {
  if (busy) return;
  $("err").hidden = true;
  setBusy(true);
  try {
    await fn();
  } catch (e) {
    $("err").textContent = e.message;
    $("err").hidden = false;
  }
  setBusy(false);
}

async function pollStatus() {
  try {
    const s = await api("/api/status");
    $("status").textContent = s.ready ? "● live" : "◌ " + s.state;
    $("status").style.color = s.ready ? css("--c-keyset") : "";
    if (!s.ready) return setTimeout(pollStatus, 3000);
    setBusy(false);
  } catch {
    $("status").textContent = "offline";
    setTimeout(pollStatus, 5000);
  }
}

(async function boot() {
  setBusy(true);
  meta = await api("/api/idempotency/meta");
  renderScenarios();
  renderScore();
  renderDocs();
  $("run").onclick = () => guarded(() => run(selected));
  $("run-all").onclick = () => guarded(async () => {
    for (const s of meta.scenarios) {
      selected = s.key;
      renderScenarios();
      await run(s.key);
    }
  });
  await pollStatus();
})();
