/* Applies the saved colour theme before first paint (no flash). Loaded
   synchronously in <head>; everything else lives in site.js. */
(function () {
  try {
    var t = localStorage.getItem("fleet-theme");
    if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
  } catch (e) { /* storage blocked: follow the OS setting */ }
})();
