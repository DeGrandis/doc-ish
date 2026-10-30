"use strict";
// The imaging viewer: a study's series, its images slice by slice, window
// and level (brightness and contrast) done here in the browser from 16-bit
// samples, zoom and pan. Loaded by app.js for DICOM studies.

const PRESETS = {
  CT: [["Soft tissue", 40, 400], ["Lung", -600, 1500], ["Bone", 400, 1800], ["Brain", 40, 80], ["Liver", 60, 160]],
};

function dicomViewer(host, d, esc, api, toast) {
  const st = { study: null, series: 0, slice: 0, cache: new Map(), wc: null, ww: null, invert: false,
    zoom: 1, panX: 0, panY: 0, tool: "slices", cur: null };
  host.innerHTML = `<div class="dv">
    <div class="dv-series" role="tablist" aria-label="Series"></div>
    <div class="dv-stage" tabindex="0" aria-label="Image. Arrow keys move through slices.">
      <canvas></canvas>
      <div class="dv-ov tl"></div><div class="dv-ov bl"></div><div class="dv-ov br"></div>
      <div class="dv-msg" hidden></div>
    </div>
    <input type="range" class="dv-slider" min="0" value="0" aria-label="Slice">
    <div class="row dv-tools">
      <div class="seg" role="radiogroup" aria-label="What dragging does">
        <button type="button" data-tool="slices">Slices</button><button type="button" data-tool="window">Contrast</button><button type="button" data-tool="pan">Move</button>
      </div>
      <select class="dv-preset" aria-label="Contrast preset"></select>
      <button type="button" data-act="invert">Invert</button>
      <button type="button" data-act="reset">Reset</button>
      <span class="spacer"></span>
      <button type="button" data-act="ask">Ask about this image</button>
    </div>
    <p class="small muted dv-help">Scroll or drag to move through slices; pick Contrast or Move to drag those instead. Ctrl+scroll or pinch to zoom.</p>
    <div class="dv-report small"></div>
  </div>`;
  const $ = s => host.querySelector(s);
  const canvas = $("canvas"), ctx = canvas.getContext("2d");
  const off = document.createElement("canvas"), offCtx = off.getContext("2d");

  function seriesNow() { return st.study.series[st.series]; }
  function instNow() { return seriesNow().instances[st.slice]; }

  async function load(inst) {
    if (st.cache.has(inst.id)) return st.cache.get(inst.id);
    const p = (async () => {
      const r = await fetch(`/api/instances/${inst.id}/pixels`);
      if (!r.ok) {
        let msg = r.statusText;
        try { msg = (await r.json()).error || msg; } catch {}
        throw new Error(msg);
      }
      const meta = JSON.parse(r.headers.get("X-Image") || "{}");
      if (meta.color) return { meta, bitmap: await createImageBitmap(await r.blob()) };
      return { meta, data: new Uint16Array(await r.arrayBuffer()) };
    })();
    st.cache.set(inst.id, p);
    p.catch(() => st.cache.delete(inst.id));
    if (st.cache.size > 300) st.cache.delete(st.cache.keys().next().value);
    return p;
  }

  // paint redraws the current image with the current window, zoom and pan.
  let lut = null, lutKey = "";
  function paint() {
    const img = st.cur;
    const w = canvas.clientWidth, h = canvas.clientHeight;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.round(w * dpr); canvas.height = Math.round(h * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.fillStyle = "#000"; ctx.fillRect(0, 0, w, h);
    if (!img) return;
    const m = img.meta;
    if (img.data) {
      const wc = st.wc ?? m.center, ww = Math.max(1, st.ww ?? m.width);
      const inv = m.invert !== st.invert;
      const key = [wc, ww, inv, m.offset, m.scale].join();
      if (key !== lutKey) {
        lut = new Uint8ClampedArray(65536);
        const lo = wc - ww / 2;
        for (let s = 0; s < 65536; s++) {
          const v = s / m.scale + m.offset;
          let g = (v - lo) / ww * 255;
          if (inv) g = 255 - g;
          lut[s] = g;
        }
        lutKey = key;
      }
      off.width = m.cols; off.height = m.rows;
      const id = offCtx.createImageData(m.cols, m.rows);
      const px = id.data, src = img.data;
      for (let i = 0, j = 0; i < src.length; i++, j += 4) {
        const g = lut[src[i]];
        px[j] = px[j + 1] = px[j + 2] = g; px[j + 3] = 255;
      }
      offCtx.putImageData(id, 0, 0);
    }
    const iw = m.cols, ih = m.rows;
    const fit = Math.min(w / iw, h / ih) * st.zoom;
    const dw = iw * fit, dh = ih * fit;
    ctx.imageSmoothingEnabled = fit < 2;
    if (st.invert && img.bitmap) ctx.filter = "invert(1)";
    ctx.drawImage(img.bitmap || off, (w - dw) / 2 + st.panX, (h - dh) / 2 + st.panY, dw, dh);
    ctx.filter = "none";
    const s = seriesNow();
    $(".tl").textContent = `${s.description || "Series " + s.number}`;
    $(".bl").textContent = s.instances.length > 1 ? `Image ${st.slice + 1} of ${s.instances.length}` : "";
    $(".br").textContent = img.data ? `C ${Math.round(st.wc ?? m.center)} W ${Math.round(st.ww ?? m.width)}${st.zoom !== 1 ? ` · ${Math.round(st.zoom * 100)}%` : ""}` : "";
  }

  let showing = 0;
  async function show() {
    const s = seriesNow(), inst = instNow(), want = ++showing;
    $(".dv-slider").max = String(s.instances.length - 1);
    $(".dv-slider").value = String(st.slice);
    $(".dv-slider").hidden = s.instances.length < 2;
    try {
      const img = await load(inst);
      if (want !== showing) return;
      st.cur = img;
      $(".dv-msg").hidden = true;
      paint();
      // Fetch the neighbours so scrolling is smooth.
      for (const k of [1, -1, 2, -2, 3]) {
        const n = s.instances[st.slice + k];
        if (n) load(n).catch(() => {});
      }
    } catch (err) {
      if (want !== showing) return;
      st.cur = null; paint();
      $(".dv-msg").hidden = false;
      $(".dv-msg").textContent = err.message;
    }
  }

  function step(n) {
    const len = seriesNow().instances.length;
    const next = Math.max(0, Math.min(len - 1, st.slice + n));
    if (next !== st.slice) { st.slice = next; show(); }
  }

  function selectSeries(i) {
    st.series = i;
    st.slice = Math.floor(seriesNow().instances.length / 2);
    st.wc = st.ww = null;
    host.querySelectorAll("[data-series]").forEach(b => b.classList.toggle("on", +b.dataset.series === i));
    setTool(seriesNow().instances.length > 1 ? "slices" : "window");
    fillPresets();
    show();
  }

  function setTool(t) {
    st.tool = t;
    host.querySelectorAll("[data-tool]").forEach(b => b.classList.toggle("on", b.dataset.tool === t));
  }

  function fillPresets() {
    const mod = (seriesNow().modality || st.study.modality || "").toUpperCase();
    const opts = [["The image's own", "", ""], ...(PRESETS[mod] || []).map(p => [p[0], p[1], p[2]])];
    $(".dv-preset").innerHTML = opts.map(([n, c, w]) => `<option value="${c},${w}">${esc(n)}</option>`).join("");
  }

  // ---------- pointer: drag, wheel, pinch ----------

  const pointers = new Map();
  let start = null, pinch = null, acc = 0;
  canvas.addEventListener("pointerdown", e => {
    canvas.setPointerCapture(e.pointerId);
    pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    const m = st.cur && st.cur.meta;
    start = { x: e.clientX, y: e.clientY, wc: st.wc ?? (m && m.center), ww: st.ww ?? (m && m.width), panX: st.panX, panY: st.panY,
      tool: e.shiftKey || e.button === 1 ? "pan" : e.button === 2 ? "window" : st.tool };
    acc = 0;
    if (pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      pinch = { dist: Math.hypot(a.x - b.x, a.y - b.y), zoom: st.zoom };
    }
  });
  canvas.addEventListener("pointermove", e => {
    if (!pointers.has(e.pointerId)) return;
    pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (pinch && pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      st.zoom = Math.max(0.5, Math.min(8, pinch.zoom * Math.hypot(a.x - b.x, a.y - b.y) / pinch.dist));
      paint();
      return;
    }
    if (!start) return;
    const dx = e.clientX - start.x, dy = e.clientY - start.y;
    if (start.tool === "pan") { st.panX = start.panX + dx; st.panY = start.panY + dy; paint(); }
    else if (start.tool === "window" && st.cur && st.cur.data) {
      const base = Math.max(1, start.ww);
      st.ww = Math.max(1, start.ww + dx * base / 200);
      st.wc = start.wc + dy * base / 200;
      paint();
    } else if (start.tool === "slices") {
      const n = Math.trunc((dy - acc) / 8);
      if (n) { acc += n * 8; step(n); }
    }
  });
  const end = e => { pointers.delete(e.pointerId); if (pointers.size < 2) pinch = null; if (!pointers.size) start = null; };
  canvas.addEventListener("pointerup", end);
  canvas.addEventListener("pointercancel", end);
  canvas.addEventListener("contextmenu", e => e.preventDefault());
  canvas.addEventListener("wheel", e => {
    e.preventDefault();
    if (e.ctrlKey) { st.zoom = Math.max(0.5, Math.min(8, st.zoom * (e.deltaY < 0 ? 1.15 : 1 / 1.15))); paint(); }
    else step(e.deltaY > 0 ? 1 : -1);
  }, { passive: false });
  $(".dv-stage").addEventListener("keydown", e => {
    if (e.key === "ArrowDown" || e.key === "ArrowRight") { e.preventDefault(); step(1); }
    if (e.key === "ArrowUp" || e.key === "ArrowLeft") { e.preventDefault(); step(-1); }
  });
  $(".dv-slider").addEventListener("input", e => { st.slice = +e.target.value; show(); });
  host.querySelector(".dv-tools").addEventListener("click", e => {
    const t = e.target.closest("[data-tool]");
    if (t) setTool(t.dataset.tool);
    const a = e.target.closest("[data-act]");
    if (!a) return;
    if (a.dataset.act === "invert") { st.invert = !st.invert; a.classList.toggle("on", st.invert); paint(); }
    if (a.dataset.act === "reset") { st.wc = st.ww = null; st.zoom = 1; st.panX = st.panY = 0; $(".dv-preset").value = ","; paint(); }
    if (a.dataset.act === "ask") host.dispatchEvent(new CustomEvent("ask-image", { bubbles: true }));
  });
  $(".dv-preset").addEventListener("change", e => {
    const [c, w] = e.target.value.split(",");
    st.wc = c === "" ? null : +c; st.ww = w === "" ? null : +w;
    paint();
  });
  const ro = new ResizeObserver(() => paint());
  ro.observe($(".dv-stage"));

  // ---------- the report ----------

  function renderReport() {
    const s = st.study, el = $(".dv-report");
    if (s.report) {
      el.innerHTML = `Report: <a href="#home/doc/${s.report.id}">${esc(s.report.title)}</a> · <button type="button" class="link" data-unlink>Unlink</button>`;
      return;
    }
    const docs = (typeof state !== "undefined" ? state.docs : []).filter(x => x.mime !== "application/dicom");
    el.innerHTML = `No report linked. <select data-link aria-label="Link a report"><option value="">Link a report…</option>${docs.map(x =>
      `<option value="${x.id}">${esc(x.title || x.filename)}${x.doc_date ? " · " + esc(x.doc_date) : ""}</option>`).join("")}</select>`;
  }
  $(".dv-report").addEventListener("change", async e => {
    const sel = e.target.closest("[data-link]");
    if (!sel || !sel.value) return;
    try { await api("PUT", `/api/studies/${st.study.id}/report`, { report_id: +sel.value }); await refresh(); toast("Report linked"); }
    catch (err) { toast(err.message, true); }
  });
  $(".dv-report").addEventListener("click", async e => {
    if (!e.target.closest("[data-unlink]")) return;
    try { await api("PUT", `/api/studies/${st.study.id}/report`, { report_id: 0 }); await refresh(); }
    catch (err) { toast(err.message, true); }
  });

  async function refresh() {
    st.study = await api("GET", `/api/documents/${d.id}/study`);
    renderReport();
  }

  (async () => {
    try {
      await refresh();
    } catch (err) { host.innerHTML = `<p class="small muted">${esc(err.message)}</p>`; return; }
    const ser = st.study.series || [];
    if (!ser.length) { host.innerHTML = `<p class="small muted">This study has no images.</p>`; return; }
    $(".dv-series").innerHTML = ser.map((s, i) => `<button type="button" role="tab" data-series="${i}" title="${esc(s.description)}">
      <img src="/api/documents/${d.id}/thumb?series=${i}" alt="" loading="lazy"><span>${esc(s.description || "Series " + s.number)} · ${s.instances.length}</span></button>`).join("");
    $(".dv-series").hidden = ser.length < 2;
    $(".dv-series").addEventListener("click", e => {
      const b = e.target.closest("[data-series]");
      if (b) selectSeries(+b.dataset.series);
    });
    // Start on the series with the most images: usually the main one.
    let best = 0;
    ser.forEach((s, i) => { if (s.instances.length > ser[best].instances.length) best = i; });
    selectSeries(best);
  })();
  return () => ro.disconnect();
}
