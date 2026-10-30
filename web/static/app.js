"use strict";
// The whole UI: sign-in, the person picker, Home (chat + summary or an
// open file), Files, Profile and Settings. Plain JavaScript, no build step.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = s => String(s ?? "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

const state = {
  people: [],
  person: null,     // the profile being viewed, with .weight
  docs: [],
  chatId: 0,
  sending: false,
  viewing: 0,       // the open document's id
};

// ---------- API ----------

async function api(method, url, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(url, opt);
  let data = null;
  try { data = await r.json(); } catch {}
  if (r.status === 401 && !url.startsWith("/api/login") && !url.startsWith("/api/recover")) {
    boot();
    throw new Error((data && data.error) || "sign in first");
  }
  if (!r.ok) throw new Error((data && data.error) || r.statusText);
  return data;
}

let toastTimer;
function toast(msg, err) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.toggle("err", !!err);
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), err ? 6000 : 3500);
}

// ---------- formatting ----------

const fmtDate = s => (s ? s.slice(0, 10) : "");
function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + " KB";
  if (n < 1024 ** 3) return (n / 1024 / 1024).toFixed(1) + " MB";
  return (n / 1024 ** 3).toFixed(2) + " GB";
}
function ageFrom(born) {
  const b = new Date(born + "T00:00:00");
  if (isNaN(b)) return null;
  const n = new Date();
  let a = n.getFullYear() - b.getFullYear();
  if (n.getMonth() < b.getMonth() || (n.getMonth() === b.getMonth() && n.getDate() < b.getDate())) a--;
  return a;
}
const initial = name => (name || "?").trim().charAt(0).toUpperCase();
function avatar(p) {
  return `<span class="avatar" style="background:${esc(p.color || "#888")}">${esc(initial(p.name))}</span>`;
}
// A short label for a file's type, for the little tile beside its name.
function typeTag(d) {
  const m = d.mime || "";
  if (m === "application/pdf") return "PDF";
  if (m === "application/dicom") return "DCM";
  if (m.startsWith("image/")) return "IMG";
  if (m === "text/csv") return "CSV";
  if (m.includes("xml")) return "XML";
  if (m.startsWith("text/")) return "TXT";
  const ext = (d.filename.split(".").pop() || "").slice(0, 4);
  return ext || "FILE";
}
const KINDS = ["lab", "imaging", "visit", "prescription", "vaccination", "bill", "device", "other"];
// statusTag is the badge for a file: what it is once read, else where it is.
function statusTag(d) {
  const tag = {
    new: ["waiting to be read", "new"], reading: ["reading…", "reading"],
    review: ["needs a look", "review"], failed: ["couldn't read", "failed"],
  }[d.status];
  const [label, cls] = tag || [d.kind || "stored", ""];
  return `<span class="kind ${cls}">${esc(label)}</span>`;
}

// A value is out of range when flagged (H, L, A…) or outside its printed range.
function outOfRange(v) {
  const f = (v.flag || "").trim().toUpperCase();
  if (f && f !== "N" && f !== "NORMAL") return true;
  if (v.value == null) return false;
  return (v.ref_low != null && v.value < v.ref_low) || (v.ref_high != null && v.value > v.ref_high);
}
function fmtNum(n) { return n == null ? "" : String(+(+n).toFixed(4)); }
function valuesTable(vals) {
  if (!vals || !vals.length) return "";
  return `<div class="scroll"><table class="values"><thead><tr><th>Result</th><th class="num">Value</th><th>Range</th><th>Date</th></tr></thead><tbody>` +
    vals.map(v => {
      const bad = outOfRange(v);
      const range = v.ref_low != null || v.ref_high != null ? `${fmtNum(v.ref_low)}–${fmtNum(v.ref_high)}` : "";
      return `<tr class="${bad ? "out" : ""}"><td>${esc(v.name)}</td>
        <td class="num">${esc(v.value != null ? fmtNum(v.value) : v.value_text)} <span class="muted">${esc(v.unit)}</span>${bad ? ` <span class="flag">${esc(v.flag || "out of range")}</span>` : ""}</td>
        <td class="muted">${esc(range)}</td><td class="muted">${esc(v.observed || v.date || "")}</td></tr>`;
    }).join("") + "</tbody></table></div>";
}

// ---------- gate: setup, sign in, recover ----------

function showGateForm(id) {
  $("#shell").hidden = true;
  $("#gate").hidden = false;
  for (const f of ["setup-form", "recovery-show", "login-form", "recover-form"]) $("#" + f).hidden = f !== id;
  const first = $(`#${id} input`);
  if (first) setTimeout(() => first.focus(), 0);
}

async function boot() {
  let s;
  try { s = await (await fetch("/api/state")).json(); }
  catch { document.body.textContent = "Can't reach the server."; return; }
  if (!s.setup) return showGateForm("setup-form");
  if (!s.authed) {
    $("#login-locked").hidden = s.unlocked;
    return showGateForm("login-form");
  }
  startApp();
}

function formError(form, msg) { $(".err", form).textContent = msg || ""; }

$("#setup-form").addEventListener("submit", async e => {
  e.preventDefault();
  const f = e.target, d = Object.fromEntries(new FormData(f));
  if (d.password !== d.confirm) return formError(f, "The passwords don't match.");
  formError(f, "");
  try {
    const r = await api("POST", "/api/setup", { name: d.name, password: d.password });
    $("#recovery-key").textContent = r.recovery;
    showGateForm("recovery-show");
  } catch (err) { formError(f, err.message); }
});
$("#recovery-ok").addEventListener("change", e => ($("#recovery-done").disabled = !e.target.checked));
$("#recovery-copy").addEventListener("click", async () => {
  try { await navigator.clipboard.writeText($("#recovery-key").textContent); toast("Copied"); } catch { toast("Select it and copy it by hand", true); }
});
$("#recovery-done").addEventListener("click", () => { $("#recovery-key").textContent = ""; startApp(); });

$("#login-form").addEventListener("submit", async e => {
  e.preventDefault();
  const f = e.target;
  formError(f, "");
  const btn = $("button[type=submit]", f);
  btn.disabled = true;
  btn.textContent = "Unlocking…";
  try {
    await api("POST", "/api/login", { password: f.password.value });
    f.reset();
    startApp();
  } catch (err) { formError(f, err.message); }
  btn.disabled = false;
  btn.textContent = "Sign in";
});
$("#to-recover").addEventListener("click", () => showGateForm("recover-form"));
$("#to-login").addEventListener("click", () => showGateForm("login-form"));
$("#recover-form").addEventListener("submit", async e => {
  e.preventDefault();
  const f = e.target;
  formError(f, "");
  try {
    await api("POST", "/api/recover", { recovery: f.recovery.value, password: f.password.value });
    f.reset();
    toast("New password set");
    startApp();
  } catch (err) { formError(f, err.message); }
});

// ---------- the app ----------

async function startApp() {
  $("#gate").hidden = true;
  $("#shell").hidden = false;
  await loadPeople();
  let want = 0;
  try { want = +localStorage.getItem("person") || 0; } catch {}
  const p = state.people.find(x => x.id === want) || state.people[0];
  await selectPerson(p ? p.id : 0, true);
  route();
}

async function loadPeople() {
  state.people = await api("GET", "/api/people");
  renderPicker();
}

function renderPicker() {
  const btn = $("#person-btn");
  const p = state.person || state.people[0];
  if (!p) return;
  $("#person-avatar").outerHTML = avatar(p).replace('class="avatar"', 'class="avatar" id="person-avatar"');
  $("#person-name").textContent = p.name;
  const many = state.people.length > 1;
  btn.classList.toggle("single", !many);
  btn.tabIndex = many ? 0 : -1;
  $("#person-menu").innerHTML = state.people.map(x =>
    `<button role="menuitem" data-person="${x.id}" class="${x.id === p.id ? "on" : ""}">${avatar(x)} ${esc(x.name)}</button>`).join("") +
    `<hr><button role="menuitem" data-go="settings">Add or manage people…</button>`;
}

$("#person-btn").addEventListener("click", e => {
  e.stopPropagation();
  const m = $("#person-menu");
  m.hidden = !m.hidden;
  $("#person-btn").setAttribute("aria-expanded", String(!m.hidden));
});
document.addEventListener("click", () => { $("#person-menu").hidden = true; $("#person-btn").setAttribute("aria-expanded", "false"); });
$("#person-menu").addEventListener("click", async e => {
  const b = e.target.closest("button");
  if (!b) return;
  if (b.dataset.go) { location.hash = "#" + b.dataset.go; return; }
  await selectPerson(+b.dataset.person);
  if (location.hash.startsWith("#home/doc/")) location.hash = "#home";
  else route();
});

// selectPerson switches every page to this profile.
async function selectPerson(id, quiet) {
  if (!id) return;
  state.person = await api("GET", `/api/people/${id}`);
  try { localStorage.setItem("person", String(id)); } catch {}
  state.chatId = 0;
  renderPicker();
  await Promise.all([loadDocs(), loadChats()]);
  renderChatEmpty();
  if (!quiet) toast(`Viewing ${state.person.name}'s records`);
}

async function loadDocs() {
  state.docs = await api("GET", `/api/people/${state.person.id}/documents`);
  badges();
  poll();
}

function badges() {
  const n = s => state.docs.filter(d => d.status === s).length;
  const look = n("review") + n("failed");
  $("#review-count").hidden = !look;
  $("#review-count").textContent = look;
  $("#reading-dot").hidden = !(n("new") + n("reading"));
}

// ---------- the reader's progress ----------

// While files are waiting to be read, check every couple of seconds; when
// one finishes, refresh what's on screen and say so in the chat.
let pollTimer = 0, polling = false;
async function poll() {
  clearTimeout(pollTimer);
  if (polling || !state.person || $("#shell").hidden) return;
  polling = true;
  let s = null;
  try { s = await api("GET", "/api/status"); } catch {}
  polling = false;
  if (!s) { pollTimer = setTimeout(poll, 15000); return; }
  state.reader = s.reader || {};
  const mine = s.counts[state.person.id] || {};
  const busy = (mine.new || 0) + (mine.reading || 0);
  const before = new Map(state.docs.map(d => [d.id, d.status]));
  const local = {};
  for (const d of state.docs) local[d.status] = (local[d.status] || 0) + 1;
  const changed = busy || Object.keys({ ...mine, ...local }).some(k => (mine[k] || 0) !== (local[k] || 0));
  if (changed) {
    state.docs = await api("GET", `/api/people/${state.person.id}/documents`);
    badges();
    const finished = state.docs.filter(d => ["new", "reading"].includes(before.get(d.id)) && !["new", "reading"].includes(d.status));
    for (const d of finished) announce(d);
    refreshView();
  }
  readerNote(busy);
  pollTimer = setTimeout(poll, busy ? 2000 : 20000);
}

// announce tells the chat what the reader made of a file.
async function announce(d) {
  if (document.body.dataset.tab !== "home") return;
  let v;
  try { v = await api("GET", `/api/documents/${d.id}`); } catch { return; }
  const name = esc(v.title || v.filename);
  const link = `<a href="#home/doc/${v.id}">${name}</a>`;
  let html;
  if (v.status === "done") {
    const out = v.values.filter(outOfRange).length;
    html = `Read ${link}${v.kind ? ` (${esc(v.kind)})` : ""}` +
      (v.values.length ? `: ${v.values.length} value${v.values.length > 1 ? "s" : ""}${out ? `, <b class="neg">${out} out of range</b>` : ""}.` : ".");
  } else if (v.status === "review") {
    html = `Read ${link}, but it needs a look before it's filed. <a href="#review">Review</a>`;
  } else {
    html = `Couldn't read ${esc(v.filename)}: ${esc(v.review_reason)}`;
  }
  const el = addMsg("note", "");
  el.innerHTML = html;
}

function readerNote(busy) {
  const r = state.reader || {};
  const el = $("#reader-note");
  if (r.error) { el.hidden = false; el.innerHTML = `Reading is paused: ${esc(r.error)} <a href="#settings">Settings</a>`; }
  else if (busy) { el.hidden = false; el.textContent = `Reading ${busy} file${busy > 1 ? "s" : ""}…`; }
  else el.hidden = true;
  const rs = $("#reader-status");
  rs.textContent = r.error ? `Reading files is paused: ${r.error}` : r.reading ? "Reading a file now." : "";
  rs.classList.toggle("neg", !!r.error);
}

// refreshView redraws whatever page is showing after files change.
function refreshView() {
  const tab = document.body.dataset.tab;
  if (tab === "home") {
    if (state.viewing) {
      const d = state.docs.find(x => x.id === state.viewing);
      if (d && $("#viewer").dataset.status !== d.status) openDoc(state.viewing);
    } else renderSummary();
  }
  if (tab === "files") renderFiles();
  if (tab === "review") renderReview();
}

// ---------- routing ----------

function route() {
  const h = location.hash.slice(1) || "home";
  const [tab, sub, id] = h.split("/");
  const known = ["home", "files", "review", "activity", "profile", "settings"];
  const t = known.includes(tab) ? tab : "home";
  $$(".tab").forEach(s => s.classList.toggle("on", s.id === t));
  $$("#tabs button").forEach(b => b.classList.toggle("on", b.dataset.tab === t));
  $("#settings-link").classList.toggle("on", t === "settings");
  document.body.dataset.tab = t;
  if (!state.person) return;
  if (t === "home") {
    renderSummary();
    if (sub === "doc" && +id) openDoc(+id); else closeViewer();
  }
  if (t === "files") renderFiles();
  if (t === "review") renderReview();
  if (t === "activity") renderActivity();
  if (t === "profile") fillProfile();
  if (t === "settings") loadSettings();
}
window.addEventListener("hashchange", route);
$("#tabs").addEventListener("click", e => {
  const b = e.target.closest("button[data-tab]");
  if (b) location.hash = "#" + b.dataset.tab;
});

// ---------- theme ----------

$("#theme-toggle").addEventListener("click", () => {
  const t = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = t;
  try { localStorage.setItem("theme", t); } catch {}
});

// ---------- Home: summary ----------

function renderSummary() {
  const p = state.person, pr = p.profile || {};
  const bits = [];
  const age = pr.born ? ageFrom(pr.born) : null;
  if (age !== null) bits.push(`${age} years`);
  if (pr.sex) bits.push(pr.sex);
  if (pr.blood_type && pr.blood_type !== "Don't know") bits.push(pr.blood_type);
  const chips = [];
  for (const a of pr.allergies || []) chips.push(`<span class="chip warn" title="Allergy">${esc(a.name)}</span>`);
  for (const c of pr.conditions || []) chips.push(`<span class="chip">${esc(c.name)}</span>`);
  const meds = (pr.medications || []).length;
  const empty = !pr.born && !(pr.allergies || []).length && !(pr.conditions || []).length && !meds;
  const recent = state.docs.slice(0, 8);
  $("#summary").innerHTML = `
    <div class="who">${avatar(p)}<div><h2>${esc(p.name)}</h2><div class="sub">${esc(bits.join(" · ") || "Profile")}</div></div></div>
    ${chips.length ? `<div class="chips">${chips.join("")}</div>` : ""}
    <div class="summary-more">
      ${meds ? `<div class="small muted">${meds} medication${meds > 1 ? "s" : ""}${p.weight ? ` · weight ${esc(p.weight.value)} ${esc(p.weight.unit)} on ${esc(p.weight.observed)}` : ""}</div>` :
        p.weight ? `<div class="small muted">Weight ${esc(p.weight.value)} ${esc(p.weight.unit)} on ${esc(p.weight.observed)}</div>` : ""}
      ${(() => {
        const n = state.docs.filter(d => d.status === "review" || d.status === "failed").length;
        return n ? `<p class="hint"><a href="#review">${n} file${n > 1 ? "s" : ""}</a> need${n > 1 ? "" : "s"} a look before ${n > 1 ? "they're" : "it's"} filed.</p>` : "";
      })()}
      ${empty ? `<p class="hint">The profile is empty. <a href="#profile">Fill it in</a>: allergies, conditions, medications. The assistant uses it in every answer.</p>` : ""}
      <span class="eyebrow">Recent files</span>
      ${recent.length ? `<ul class="doc-list">${recent.map(docItem).join("")}</ul>
        ${state.docs.length > recent.length ? `<a class="small" href="#files">All ${state.docs.length} files</a>` : ""}`
        : `<p class="small muted">No files yet. Drop anything medical onto this window, or use the paperclip beside the chat.</p>`}
    </div>`;
}

function docItem(d) {
  const right = d.status === "done" ? `<span class="d">${esc(d.doc_date || fmtDate(d.added))}</span>` : statusTag(d);
  return `<li><button data-doc="${d.id}"><span class="ficon">${esc(typeTag(d))}</span><span class="t">${esc(d.title || d.filename)}</span>${right}</button></li>`;
}

$("#summary").addEventListener("click", e => {
  const b = e.target.closest("[data-doc]");
  if (b) location.hash = `#home/doc/${b.dataset.doc}`;
});

// ---------- Home: viewer ----------

async function openDoc(id) {
  let d;
  try { d = await api("GET", `/api/documents/${id}`); }
  catch (err) { toast(err.message, true); location.hash = "#home"; return; }
  if (d.person_id !== state.person.id) {
    const owner = state.people.find(p => p.id === d.person_id);
    if (owner) { await selectPerson(owner.id, true); renderSummary(); }
  }
  state.viewing = id;
  $(".home").classList.add("viewing");
  $("#summary").hidden = true;
  const v = $("#viewer");
  v.hidden = false;
  v.dataset.status = d.status;
  const src = `/api/documents/${id}/file`;
  const sub = [d.kind, d.doc_date, d.provider, fmtSize(d.size), d.filename !== (d.title || d.filename) ? d.filename : ""].filter(Boolean).join(" · ");
  const found = d.status === "review" && d.result ? (d.result.observations || []) : d.values;
  v.innerHTML = `
    <div class="viewer-head">
      <span class="ficon">${esc(typeTag(d))}</span>
      <div class="spacer"><h2>${esc(d.title || d.filename)}</h2><div class="sub">${esc(sub)}</div></div>
      <a class="icon-btn" href="${src}?download=1" title="Download" aria-label="Download"><svg viewBox="0 0 20 20"><path d="M10 3v10M5 9l5 5 5-5M4 17h12"/></svg></a>
      <button class="icon-btn" id="viewer-close" title="Close" aria-label="Close"><svg viewBox="0 0 20 20"><path d="M5 5l10 10M15 5L5 15"/></svg></button>
    </div>
    ${statusBox(d)}
    ${d.summary ? `<p class="viewer-summary">${esc(d.summary)}</p>` : ""}
    ${found && found.length ? `<span class="eyebrow">${d.status === "review" ? "Found, not filed yet" : "Values"}</span>${valuesTable(found)}` : ""}
    ${d.result && d.result.medications && d.result.medications.length ? `<span class="eyebrow">Medications mentioned</span><ul class="meds">${d.result.medications.map(m => `<li>${esc(m.name)}${m.detail ? ` <span class="muted">${esc(m.detail)}</span>` : ""}</li>`).join("")}</ul>` : ""}
    <div class="viewer-body" id="viewer-body"></div>
    <details class="viewer-meta">
      <summary>Details</summary>
      <form id="doc-form" class="grid">
        <label class="wide">Title <input name="title" value="${esc(d.title)}" placeholder="${esc(d.filename)}"></label>
        <label>Kind <select name="kind"><option value=""></option>${KINDS.map(k => `<option ${k === d.kind ? "selected" : ""}>${k}</option>`).join("")}</select></label>
        <label>Date <input name="doc_date" type="date" value="${esc(d.doc_date)}"></label>
        ${state.people.length > 1 ? `<label>Belongs to <select name="person_id">${state.people.map(p => `<option value="${p.id}" ${p.id === d.person_id ? "selected" : ""}>${esc(p.name)}</option>`).join("")}</select></label>` : ""}
        <div class="wide row"><button class="primary" type="submit">Save</button>${["done", "review", "failed"].includes(d.status) ? `<button type="button" data-reread="${d.id}">Read again</button>` : ""}<span class="spacer"></span><button type="button" class="danger" id="doc-delete">Delete file</button></div>
        <p class="wide small muted">Added ${esc(fmtDate(d.added))} · SHA-256 <span class="mono">${esc(d.sha256.slice(0, 16))}…</span></p>
      </form>
    </details>`;
  const body = $("#viewer-body");
  const m = d.mime || "";
  if (m.startsWith("image/") && m !== "image/heic") body.innerHTML = `<img src="${src}" alt="${esc(d.title || d.filename)}">`;
  else if (m === "application/pdf") body.innerHTML = `<iframe src="${src}" title="${esc(d.filename)}"></iframe>`;
  else if (!m.includes("officedocument") && (m.startsWith("text/") || m.includes("json") || m.includes("xml"))) {
    body.innerHTML = `<p class="small muted">Loading…</p>`;
    const text = await (await fetch(src)).text();
    if (state.viewing !== id) return;
    body.innerHTML = m === "text/csv" ? csvTable(text) : `<pre>${esc(text.slice(0, 200000))}</pre>`;
  } else {
    body.innerHTML = `<p class="small muted">${m === "application/dicom" ? "Images get a viewer in a later version." : "This kind of file can't be shown here."} Download it to open it.</p>`;
  }
}

// statusBox says where a file stands, with what can be done about it.
function statusBox(d) {
  if (d.status === "new") return `<p class="status-box">Waiting to be read.${state.reader && state.reader.error ? ` Reading is paused: ${esc(state.reader.error)}` : ""}</p>`;
  if (d.status === "reading") return `<p class="status-box"><span class="typing"></span> Reading it now…</p>`;
  if (d.status === "failed") return `<div class="status-box bad"><p>Couldn't read it: ${esc(d.review_reason)}</p><div class="row"><button data-reread="${d.id}">Read again</button></div></div>`;
  if (d.status !== "review") return "";
  const better = moveTarget(d);
  return `<div class="status-box look">
    <p><b>Needs a look before it's filed.</b></p>
    <ul>${d.review_reason.split("\n").filter(Boolean).map(r => `<li>${esc(r)}</li>`).join("")}</ul>
    <div class="row">
      <button class="primary" data-accept="${d.id}">Looks right, file it</button>
      ${better ? `<button data-move="${d.id}" data-to="${better.id}">Move to ${esc(better.name)}</button>` : ""}
      <button data-reread="${d.id}">Read again</button>
    </div></div>`;
}

// moveTarget is the other profile the reader thinks a file belongs to.
function moveTarget(d) {
  return state.people.find(p => p.id !== d.person_id && d.review_reason.includes(`like ${p.name}'s`));
}

// The buttons in status boxes and Review cards work the same everywhere.
async function docAction(e) {
  const b = e.target.closest("[data-accept],[data-reread],[data-move]");
  if (!b) return false;
  b.disabled = true;
  try {
    if (b.dataset.accept) {
      const v = await api("POST", `/api/documents/${b.dataset.accept}/accept`);
      toast(`Filed${v.values.length ? ` with ${v.values.length} value${v.values.length > 1 ? "s" : ""}` : ""}`);
    } else if (b.dataset.reread) {
      await api("POST", `/api/documents/${b.dataset.reread}/read`);
      toast("Reading it again");
    } else if (b.dataset.move) {
      const to = state.people.find(p => p.id === +b.dataset.to);
      await api("PATCH", `/api/documents/${b.dataset.move}`, { person_id: to.id });
      await api("POST", `/api/documents/${b.dataset.move}/accept`).catch(() => {});
      toast(`Moved to ${to.name} and filed`);
      if (state.viewing === +b.dataset.move) location.hash = "#home";
    }
  } catch (err) { toast(err.message, true); b.disabled = false; return true; }
  await loadDocs();
  if (state.viewing && document.body.dataset.tab === "home") openDoc(state.viewing);
  refreshView();
  return true;
}
document.addEventListener("click", e => { if (e.target.closest("#viewer, #review-list")) docAction(e); });

// ---------- Review ----------

async function renderReview() {
  $("#review-title").textContent = `Review · ${state.person.name}`;
  const list = await api("GET", `/api/people/${state.person.id}/review`);
  const el = $("#review-list");
  if (!list.length) { el.innerHTML = `<p class="muted">Nothing waiting. Every file is read and filed.</p>`; return; }
  el.innerHTML = list.map(d => {
    const vals = d.result ? d.result.observations || [] : [];
    return `<article class="review-card">
      <div class="viewer-head"><span class="ficon">${esc(typeTag(d))}</span>
        <div class="spacer"><h2><a href="#home/doc/${d.id}">${esc(d.title || d.filename)}</a></h2>
        <div class="sub">${esc([d.kind, d.doc_date, d.provider, d.filename].filter(Boolean).join(" · "))}</div></div></div>
      ${statusBox(d)}
      ${d.summary ? `<p class="viewer-summary">${esc(d.summary)}</p>` : ""}
      ${vals.length ? valuesTable(vals.slice(0, 12)) + (vals.length > 12 ? `<p class="small muted">and ${vals.length - 12} more</p>` : "") : ""}
    </article>`;
  }).join("");
}

// ---------- Activity ----------

async function renderActivity() {
  $("#activity-title").textContent = `Activity · ${state.person.name}`;
  const list = await api("GET", `/api/people/${state.person.id}/activity`);
  const who = { you: "You", reader: "Reader", assistant: "Assistant" };
  $("#activity-list").innerHTML = list.length ? `<ul class="act-list">${list.map(a => `<li class="${a.undone ? "undone" : ""}">
      <span class="when">${esc(new Date(a.at).toLocaleString([], { dateStyle: "medium", timeStyle: "short" }))}</span>
      <span class="actor ${esc(a.actor)}">${esc(who[a.actor] || a.actor)}</span>
      <span class="what">${esc(a.summary)}${a.undone ? ` <span class="muted">(undone)</span>` : ""}</span>
      ${a.can_undo ? `<button data-undo="${a.id}">Undo</button>` : ""}
    </li>`).join("")}</ul>` : `<p class="muted">Nothing yet.</p>`;
}
$("#activity-list").addEventListener("click", async e => {
  const b = e.target.closest("[data-undo]");
  if (!b) return;
  b.disabled = true;
  try {
    await api("POST", `/api/activity/${b.dataset.undo}/undo`);
    toast("Undone");
    state.person = await api("GET", `/api/people/${state.person.id}`);
    await loadDocs();
    renderActivity();
  } catch (err) { toast(err.message, true); b.disabled = false; }
});

function csvTable(text) {
  const rows = text.split(/\r?\n/).filter(Boolean).slice(0, 500).map(parseCSVLine);
  if (!rows.length) return "<p class='muted'>Empty</p>";
  return `<div class="csv"><table><thead><tr>${rows[0].map(c => `<th>${esc(c)}</th>`).join("")}</tr></thead><tbody>` +
    rows.slice(1).map(r => `<tr>${r.map(c => `<td>${esc(c)}</td>`).join("")}</tr>`).join("") + "</tbody></table></div>";
}
function parseCSVLine(line) {
  const out = []; let cur = "", q = false;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (q) {
      if (ch === '"' && line[i + 1] === '"') { cur += '"'; i++; }
      else if (ch === '"') q = false;
      else cur += ch;
    } else if (ch === '"') q = true;
    else if (ch === ",") { out.push(cur); cur = ""; }
    else cur += ch;
  }
  out.push(cur);
  return out;
}

function closeViewer() {
  state.viewing = 0;
  $(".home").classList.remove("viewing");
  $("#viewer").hidden = true;
  $("#viewer").innerHTML = "";
  $("#summary").hidden = false;
}

$("#viewer").addEventListener("click", async e => {
  if (e.target.closest("#viewer-close")) { location.hash = "#home"; return; }
  if (e.target.closest("#doc-delete")) {
    const d = state.docs.find(x => x.id === state.viewing);
    if (!confirm(`Delete ${d ? d.filename : "this file"}? This can't be undone.`)) return;
    try {
      await api("DELETE", `/api/documents/${state.viewing}`);
      await loadDocs();
      toast("Deleted");
      location.hash = "#home";
    } catch (err) { toast(err.message, true); }
  }
});
$("#viewer").addEventListener("submit", async e => {
  if (e.target.id !== "doc-form") return;
  e.preventDefault();
  const d = Object.fromEntries(new FormData(e.target));
  if (d.person_id) d.person_id = +d.person_id;
  try {
    const saved = await api("PATCH", `/api/documents/${state.viewing}`, d);
    if (saved.person_id !== state.person.id) {
      const p = state.people.find(x => x.id === saved.person_id);
      toast(`Moved to ${p ? p.name : "another profile"}`);
      await loadDocs();
      location.hash = "#home";
      return;
    }
    await loadDocs();
    toast("Saved");
    openDoc(state.viewing);
  } catch (err) { toast(err.message, true); }
});

// ---------- Home: chat ----------

async function loadChats() {
  const chats = await api("GET", `/api/people/${state.person.id}/chats`);
  $("#chat-pick").innerHTML = `<option value="0">New chat</option>` +
    chats.map(c => `<option value="${c.id}" ${c.id === state.chatId ? "selected" : ""}>${esc(c.title || "Chat")} · ${esc(fmtDate(c.updated))}</option>`).join("");
  $("#chat-pick").hidden = !chats.length;
}

function renderChatEmpty() {
  const p = state.person;
  const first = esc(p.name.split(" ")[0]);
  const ideas = [
    "What's in my records so far?",
    "What should I add to my profile?",
    "What does an A1C test measure?",
  ];
  $("#chat-log").innerHTML = `<div class="chat-empty">
    <h2>Hi, <span class="hi">${first}</span>.</h2>
    <p class="muted">Ask about ${first === esc(p.name) ? "these" : first + "'s"} records, or drop a file anywhere to add it.</p>
    <div class="row">${ideas.map(t => `<button type="button" data-idea="${esc(t)}">${esc(t)}</button>`).join("")}</div>
  </div>`;
}

async function showChat(id) {
  state.chatId = id;
  if (!id) return renderChatEmpty();
  const msgs = await api("GET", `/api/chats/${id}`);
  const log = $("#chat-log");
  log.innerHTML = "";
  for (const m of msgs) addMsg(m.role, m.content);
  log.scrollTop = log.scrollHeight;
}

function addMsg(role, text) {
  const log = $("#chat-log");
  $(".chat-empty", log)?.remove();
  const el = document.createElement("div");
  el.className = "msg " + role;
  if (role === "assistant") el.innerHTML = md(text);
  else el.textContent = text;
  log.appendChild(el);
  log.scrollTop = log.scrollHeight;
  return el;
}

$("#chat-pick").addEventListener("change", e => showChat(+e.target.value));
$("#chat-new").addEventListener("click", () => { $("#chat-pick").value = "0"; showChat(0); $("#chat-input").focus(); });
$("#chat-log").addEventListener("click", e => {
  const b = e.target.closest("[data-idea]");
  if (b) { $("#chat-input").value = b.dataset.idea; send(); }
});

const input = $("#chat-input");
function autosize() { input.style.height = "auto"; input.style.height = Math.min(input.scrollHeight, 200) + "px"; }
input.addEventListener("input", autosize);
input.addEventListener("keydown", e => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
});
$("#chat-form").addEventListener("submit", e => { e.preventDefault(); send(); });

async function send() {
  const text = input.value.trim();
  if (!text || state.sending) return;
  state.sending = true;
  $("#chat-send").disabled = true;
  input.value = "";
  autosize();
  addMsg("user", text);
  const out = addMsg("assistant", "");
  out.innerHTML = `<span class="typing"></span>`;
  let reply = "", queued = false;
  const paint = () => { queued = false; out.innerHTML = md(reply); $("#chat-log").scrollTop = $("#chat-log").scrollHeight; };
  try {
    const r = await fetch(`/api/people/${state.person.id}/chat`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ chat_id: state.chatId, message: text }),
    });
    if (!r.ok) {
      let msg = r.statusText;
      try { msg = (await r.json()).error || msg; } catch {}
      if (r.status === 401) boot();
      throw new Error(msg);
    }
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        if (!line.trim()) continue;
        const ev = JSON.parse(line);
        if (ev.chat_id && !state.chatId) state.chatId = ev.chat_id;
        if (ev.t) { reply += ev.t; if (!queued) { queued = true; requestAnimationFrame(paint); } }
        if (ev.error) throw new Error(ev.error);
      }
    }
    paint();
    if (!reply.trim()) out.innerHTML = `<span class="muted">No answer.</span>`;
    loadChats();
  } catch (err) {
    if (!reply) out.remove();
    const e = addMsg("assistant", "");
    e.classList.add("err");
    e.textContent = err.message;
    if (/Settings/.test(err.message)) e.innerHTML += ` <a href="#settings">Open Settings</a>`;
  }
  state.sending = false;
  $("#chat-send").disabled = false;
}

// ---------- adding files ----------

async function upload(files) {
  files = [...files].filter(f => f.size >= 0);
  if (!files.length || !state.person) return;
  const who = state.person;
  const fd = new FormData();
  for (const f of files) fd.append("file", f, f.name);
  toast(`Adding ${files.length} file${files.length > 1 ? "s" : ""} to ${who.name}'s records…`);
  let res;
  try {
    const r = await fetch(`/api/people/${who.id}/documents`, { method: "POST", body: fd });
    res = await r.json();
    if (!r.ok) throw new Error(res.error || r.statusText);
  } catch (err) { toast(err.message, true); return; }
  const added = res.filter(x => x.document && !x.duplicate);
  const dup = res.filter(x => x.duplicate);
  const bad = res.filter(x => x.error);
  await loadDocs();
  const parts = [];
  if (added.length) parts.push(`Added ${added.length === 1 ? added[0].filename : added.length + " files"} to ${who.name}'s records`);
  if (dup.length) parts.push(`${dup.length} already here`);
  if (bad.length) parts.push(`${bad.length} failed: ${bad.map(b => `${b.filename} (${b.error})`).join(", ")}`);
  toast(parts.join(". ") + ".", bad.length > 0);
  if (document.body.dataset.tab === "home") {
    if (!state.viewing) renderSummary();
    if (added.length) addMsg("note", `Added ${added.map(a => a.filename).join(", ")}. Reading ${added.length > 1 ? "them" : "it"} now…`);
  }
  if (document.body.dataset.tab === "files") renderFiles();
}

$("#attach-btn").addEventListener("click", () => $("#attach-input").click());
$("#camera-btn").addEventListener("click", () => $("#camera-input").click());
for (const id of ["#attach-input", "#camera-input"]) {
  $(id).addEventListener("change", e => { upload(e.target.files); e.target.value = ""; });
}

// Drag files anywhere onto the window.
let dragDepth = 0;
const hasFiles = e => [...(e.dataTransfer?.types || [])].includes("Files");
window.addEventListener("dragenter", e => {
  if (!hasFiles(e) || $("#shell").hidden) return;
  dragDepth++;
  $("#drop-who").textContent = `${state.person.name}'s records`;
  $("#drop-overlay").hidden = false;
});
window.addEventListener("dragover", e => { if (hasFiles(e)) e.preventDefault(); });
window.addEventListener("dragleave", e => { if (hasFiles(e) && --dragDepth <= 0) { dragDepth = 0; $("#drop-overlay").hidden = true; } });
window.addEventListener("drop", e => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth = 0;
  $("#drop-overlay").hidden = true;
  if (!$("#shell").hidden) upload(e.dataTransfer.files);
});

// ---------- Files ----------

let filesTimer;
$("#files-q").addEventListener("input", () => { clearTimeout(filesTimer); filesTimer = setTimeout(renderFiles, 200); });
$("#files-drop").addEventListener("click", () => $("#attach-input").click());
$("#files-drop").addEventListener("keydown", e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); $("#attach-input").click(); } });

async function renderFiles() {
  $("#files-title").textContent = `${state.person.name}'s files`;
  readerNote(state.docs.filter(d => d.status === "new" || d.status === "reading").length);
  const q = $("#files-q").value.trim();
  const docs = q ? await api("GET", `/api/people/${state.person.id}/documents?q=${encodeURIComponent(q)}`) : state.docs;
  $("#files-table tbody").innerHTML = docs.map(d => `<tr data-doc="${d.id}">
    <td><div class="fn"><span class="ficon">${esc(typeTag(d))}</span><span>${esc(d.title || d.filename)}</span></div></td>
    <td>${statusTag(d)}</td>
    <td>${esc(d.doc_date)}</td>
    <td>${esc(fmtDate(d.added))}</td>
    <td class="num">${esc(fmtSize(d.size))}</td></tr>`).join("");
  $("#files-table").hidden = !docs.length;
  $("#files-empty").hidden = !!docs.length;
  $("#files-empty").textContent = q ? "No files match." : "Nothing here yet.";
}
$("#files-table").addEventListener("click", e => {
  const tr = e.target.closest("tr[data-doc]");
  if (tr) location.hash = `#home/doc/${tr.dataset.doc}`;
});

// ---------- Profile ----------

const LISTS = [
  ["allergies", "Allergies", "Allergen", "Reaction", "Since"],
  ["medications", "Medications and supplements", "Name", "Dose and how often", "Since"],
  ["conditions", "Conditions", "Condition", "Details", "Since"],
  ["surgeries", "Surgeries and hospital stays", "What", "Where, why", "When"],
  ["family_history", "Family history", "Condition", "Who", "Age it started"],
  ["vaccinations", "Vaccinations", "Vaccine", "Details", "Date"],
  ["providers", "Doctors and clinics", "Name", "Specialty, phone", "Since"],
];
const SCALARS = ["born", "sex", "height", "blood_type", "lifestyle", "emergency_contact", "insurance", "notes"];

function itemRow(key, it = {}) {
  const l = LISTS.find(x => x[0] === key);
  return `<div class="item">
    <input data-f="name" value="${esc(it.name)}" placeholder="${esc(l[2])}" aria-label="${esc(l[2])}">
    <input data-f="detail" value="${esc(it.detail)}" placeholder="${esc(l[3])}" aria-label="${esc(l[3])}">
    <input data-f="since" value="${esc(it.since)}" placeholder="${esc(l[4])}" aria-label="${esc(l[4])}">
    <button type="button" class="icon-btn" data-remove title="Remove" aria-label="Remove"><svg viewBox="0 0 20 20"><path d="M5 5l10 10M15 5L5 15"/></svg></button>
  </div>`;
}

function fillProfile() {
  const p = state.person, pr = p.profile || {}, f = $("#profile-form");
  $("#profile-title").textContent = p.name;
  f.name.value = p.name;
  for (const k of SCALARS) f[k].value = pr[k] || "";
  f.weight.value = p.weight ? `${p.weight.value ?? p.weight.value_text} ${p.weight.unit}`.trim() : "";
  f.weight.dataset.was = f.weight.value;
  $("#weight-asof").textContent = p.weight ? `as of ${p.weight.observed}` : "";
  $("#profile-lists").innerHTML = LISTS.map(([key, title]) => `
    <h2 class="section-h">${esc(title)}</h2>
    <div class="plist" data-list="${key}">
      ${(pr[key] || []).map(it => itemRow(key, it)).join("")}
      <button type="button" class="plist-add" data-add="${key}">Add</button>
    </div>`).join("");
  $("#profile-saved").textContent = "";
}

$("#profile-lists").addEventListener("click", e => {
  const add = e.target.closest("[data-add]");
  if (add) {
    add.insertAdjacentHTML("beforebegin", itemRow(add.dataset.add));
    add.previousElementSibling.querySelector("input").focus();
    $("#profile-saved").textContent = "Unsaved changes";
  }
  const rm = e.target.closest("[data-remove]");
  if (rm) { rm.closest(".item").remove(); $("#profile-saved").textContent = "Unsaved changes"; }
});
$("#profile-form").addEventListener("input", () => ($("#profile-saved").textContent = "Unsaved changes"));

// parseWeight reads "182", "182 lb", "82.5kg"; no unit means pounds.
function parseWeight(s) {
  const m = s.trim().match(/^(\d+(?:[.,]\d+)?)\s*(lbs?|pounds?|kg|kilos?|kilograms?)?$/i);
  if (!m) return null;
  const unit = m[2] && /^k/i.test(m[2]) ? "kg" : "lb";
  return { value: parseFloat(m[1].replace(",", ".")), unit };
}

$("#profile-form").addEventListener("submit", async e => {
  e.preventDefault();
  const f = e.target, p = state.person;
  const profile = {};
  for (const k of SCALARS) if (f[k].value.trim()) profile[k] = f[k].value.trim();
  for (const [key] of LISTS) {
    const items = $$(`[data-list="${key}"] .item`).map(row => {
      const it = {};
      for (const inp of $$("input", row)) if (inp.value.trim()) it[inp.dataset.f] = inp.value.trim();
      return it;
    }).filter(it => it.name);
    if (items.length) profile[key] = items;
  }
  try {
    const wt = f.weight.value.trim();
    if (wt && wt !== f.weight.dataset.was) {
      const w = parseWeight(wt);
      if (!w) throw new Error("Write the weight like 182 lb or 82 kg");
      await api("POST", `/api/people/${p.id}/observations`, { name: "Weight", value: w.value, unit: w.unit, source: "profile" });
    }
    await api("PUT", `/api/people/${p.id}`, { name: f.name.value, profile });
    await loadPeople();
    state.person = await api("GET", `/api/people/${p.id}`);
    renderPicker();
    fillProfile();
    $("#profile-saved").textContent = "Saved";
  } catch (err) { toast(err.message, true); }
});

// ---------- Settings ----------

async function loadSettings() {
  const s = await api("GET", "/api/settings");
  const f = $("#ai-form");
  f.ollama_url.value = s.ollama_url;
  f.num_ctx.value = String(s.num_ctx);
  f.dataset.chat = s.chat_model;
  f.dataset.vision = s.vision_model;
  fillModelSelects([], s);
  $("#ai-status").textContent = s.ollama_found ? "Found on your server." : "";
  $("#ai-status").classList.remove("bad");
  if (s.ollama_url) checkModels(true);
  renderPeopleList();
}

function fillModelSelects(models, s) {
  const f = $("#ai-form");
  const chat = s ? s.chat_model : f.chat_model.value;
  const vision = s ? s.vision_model : f.vision_model.value;
  const opts = (want, need) => {
    const list = models.slice();
    if (want && !list.find(m => m.name === want)) list.unshift({ name: want, tools: true, vision: true, missing: true });
    return `<option value="">None</option>` + list.map(m => {
      const ok = need === "tools" ? m.tools : m.vision;
      return `<option value="${esc(m.name)}" ${m.name === want ? "selected" : ""} ${ok || m.missing ? "" : "disabled"}>${esc(m.name)}${m.missing ? " (not found)" : ""}${m.size ? ` · ${(m.size / 1e9).toFixed(1)} GB` : ""}</option>`;
    }).join("");
  };
  f.chat_model.innerHTML = opts(chat, "tools");
  f.vision_model.innerHTML = opts(vision, "vision");
}

async function checkModels(quiet) {
  const f = $("#ai-form"), st = $("#ai-status");
  const url = f.ollama_url.value.trim();
  if (!url) { st.textContent = "Enter the address first."; st.classList.add("bad"); return; }
  st.textContent = "Checking…";
  st.classList.remove("bad");
  try {
    const r = await api("GET", "/api/models?url=" + encodeURIComponent(url));
    fillModelSelects(r.models);
    const usable = r.models.filter(m => m.tools).length;
    st.textContent = `Connected: ${r.models.length} models, ${usable} can use tools.` + (r.public ? " Careful: this address is outside your network, so your records would leave it." : "");
    st.classList.toggle("bad", !!r.public);
  } catch (err) {
    st.textContent = err.message;
    st.classList.add("bad");
    if (quiet) fillModelSelects([]);
  }
}
$("#ai-check").addEventListener("click", () => checkModels());
$("#ai-form").addEventListener("submit", async e => {
  e.preventDefault();
  const f = e.target;
  try {
    await api("PUT", "/api/settings", { ollama_url: f.ollama_url.value, chat_model: f.chat_model.value, vision_model: f.vision_model.value, num_ctx: +f.num_ctx.value });
    toast("Saved");
  } catch (err) { toast(err.message, true); }
});

function renderPeopleList() {
  $("#people-list").innerHTML = state.people.map(p => `<div class="people-row">${avatar(p)}<span class="spacer">${esc(p.name)}</span>
    <button type="button" data-edit="${p.id}">Edit profile</button>
    ${state.people.length > 1 ? `<button type="button" class="danger" data-del="${p.id}">Delete</button>` : ""}</div>`).join("");
}
$("#people-list").addEventListener("click", async e => {
  const ed = e.target.closest("[data-edit]");
  if (ed) { await selectPerson(+ed.dataset.edit, true); location.hash = "#profile"; return; }
  const del = e.target.closest("[data-del]");
  if (!del) return;
  const p = state.people.find(x => x.id === +del.dataset.del);
  const typed = prompt(`This deletes ${p.name}'s profile, files and chats for good. Type their name to confirm.`);
  if (typed === null) return;
  if (typed.trim() !== p.name) { toast("The name didn't match; nothing was deleted.", true); return; }
  try {
    await api("DELETE", `/api/people/${p.id}`);
    await loadPeople();
    if (state.person.id === p.id) await selectPerson(state.people[0].id, true);
    renderPeopleList();
    toast(`Deleted ${p.name}`);
  } catch (err) { toast(err.message, true); }
});
$("#person-add").addEventListener("submit", async e => {
  e.preventDefault();
  const name = e.target.name.value.trim();
  if (!name) return;
  try {
    const p = await api("POST", "/api/people", { name });
    e.target.reset();
    await loadPeople();
    renderPeopleList();
    toast(`Added ${p.name}. Pick them from the menu at the top.`);
  } catch (err) { toast(err.message, true); }
});

$("#password-form").addEventListener("submit", async e => {
  e.preventDefault();
  const f = e.target;
  try {
    await api("POST", "/api/password", { old: f.old.value, new: f.new.value });
    f.reset();
    toast("Password changed. Other devices are signed out.");
  } catch (err) { toast(err.message, true); }
});
$("#lock-btn").addEventListener("click", async () => {
  if (!confirm("Lock now? Everyone is signed out, and the records stay locked until someone signs in.")) return;
  await api("POST", "/api/lock").catch(() => {});
  boot();
});
$("#logout-btn").addEventListener("click", async () => {
  await api("POST", "/api/logout").catch(() => {});
  boot();
});

boot();
