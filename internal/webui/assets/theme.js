// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh
//
// Loaded synchronously in <head> (CSP forbids inline scripts) so the saved
// light/dark choice is applied before first paint — no theme flash. With no
// saved choice the stylesheet follows prefers-color-scheme.
"use strict";
(function () {
  try {
    var t = window.localStorage.getItem("fleet.theme");
    if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
  } catch (e) { /* storage unavailable: follow the system theme */ }
})();
