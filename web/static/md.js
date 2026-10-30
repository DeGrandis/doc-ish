"use strict";
// A small, safe Markdown renderer for the assistant's answers: everything
// is escaped first, then headings, lists, tables, code and emphasis are
// turned back into tags. Links only for http(s).
const md = (() => {
  const esc = s => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

  function inline(s) {
    const codes = [];
    s = s.replace(/`([^`]+)`/g, (_, c) => { codes.push(c); return `\u0000${codes.length - 1}\u0000`; });
    s = s.replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>")
         .replace(/(^|[^*])\*([^*\s][^*]*)\*/g, "$1<i>$2</i>")
         .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
    return s.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${codes[+i]}</code>`);
  }

  function table(rows) {
    const cells = r => r.replace(/^\s*\|/, "").replace(/\|\s*$/, "").split("|").map(c => inline(c.trim()));
    const head = cells(rows[0]);
    const body = rows.slice(2).map(cells);
    return `<div class="scroll"><table><thead><tr>${head.map(c => `<th>${c}</th>`).join("")}</tr></thead><tbody>` +
      body.map(r => `<tr>${r.map(c => `<td>${c}</td>`).join("")}</tr>`).join("") + "</tbody></table></div>";
  }

  return function render(text) {
    const lines = esc(text || "").split("\n");
    const out = [];
    let i = 0;
    while (i < lines.length) {
      const l = lines[i];
      if (/^```/.test(l)) {
        const buf = [];
        for (i++; i < lines.length && !/^```/.test(lines[i]); i++) buf.push(lines[i]);
        i++;
        out.push(`<pre><code>${buf.join("\n")}</code></pre>`);
        continue;
      }
      const h = l.match(/^(#{1,4})\s+(.*)$/);
      if (h) { out.push(`<h${h[1].length < 3 ? 3 : 4}>${inline(h[2])}</h${h[1].length < 3 ? 3 : 4}>`); i++; continue; }
      if (/^\s*\|.*\|\s*$/.test(l) && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}/.test(lines[i + 1])) {
        const rows = [];
        while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) rows.push(lines[i++]);
        out.push(table(rows));
        continue;
      }
      const list = l.match(/^\s*([-*•]|\d+[.)])\s+/);
      if (list) {
        const ordered = /\d/.test(list[1]);
        const items = [];
        while (i < lines.length && /^\s*([-*•]|\d+[.)])\s+/.test(lines[i])) {
          items.push(inline(lines[i].replace(/^\s*([-*•]|\d+[.)])\s+/, "")));
          i++;
        }
        out.push(`<${ordered ? "ol" : "ul"}>${items.map(t => `<li>${t}</li>`).join("")}</${ordered ? "ol" : "ul"}>`);
        continue;
      }
      if (!l.trim()) { i++; continue; }
      const para = [];
      while (i < lines.length && lines[i].trim() && !/^(```|#{1,4}\s|\s*([-*•]|\d+[.)])\s+)/.test(lines[i]) && !/^\s*\|.*\|\s*$/.test(lines[i])) para.push(lines[i++]);
      if (para.length) out.push(`<p>${inline(para.join("<br>"))}</p>`);
      else out.push(`<p>${inline(lines[i++])}</p>`);
    }
    return out.join("");
  };
})();
