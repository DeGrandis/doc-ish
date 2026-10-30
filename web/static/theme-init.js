"use strict";
// Runs before the page paints: pick the remembered theme, else the OS one,
// so there is no flash of the wrong colors. app.js owns the toggle.
(() => {
  let t = null;
  try { t = localStorage.getItem("theme"); } catch {}
  if (t !== "light" && t !== "dark") t = matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  document.documentElement.dataset.theme = t;
})();
