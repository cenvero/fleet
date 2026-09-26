/* Cenvero Fleet — site behaviour: theme toggle, mobile menu, tabs, copy
   buttons, and the docs sidebar (search filter, scroll spy). No dependencies;
   no inline handlers, so pages run under a strict Content-Security-Policy. */
(function () {
  "use strict";
  var root = document.documentElement;

  /* ── Theme ─────────────────────────────────────────── */
  function currentTheme() {
    var t = root.getAttribute("data-theme");
    if (t === "light" || t === "dark") return t;
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }
  function syncThemeButtons() {
    var next = currentTheme() === "dark" ? "light" : "dark";
    document.querySelectorAll("[data-theme-toggle]").forEach(function (b) {
      b.setAttribute("aria-label", "Switch to " + next + " theme");
      b.setAttribute("title", "Switch to " + next + " theme");
    });
  }
  document.querySelectorAll("[data-theme-toggle]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var next = currentTheme() === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      try { localStorage.setItem("fleet-theme", next); } catch (e) { /* ignore */ }
      syncThemeButtons();
    });
  });
  syncThemeButtons();

  /* ── Mobile menu ───────────────────────────────────── */
  document.querySelectorAll("[data-menu-toggle]").forEach(function (btn) {
    var target = document.getElementById(btn.getAttribute("aria-controls"));
    if (!target) return;
    function setOpen(open) {
      target.classList.toggle("is-open", open);
      btn.setAttribute("aria-expanded", open ? "true" : "false");
      btn.setAttribute("aria-label", open ? "Close menu" : "Open menu");
    }
    btn.addEventListener("click", function () { setOpen(!target.classList.contains("is-open")); });
    target.addEventListener("click", function (e) { if (e.target.closest("a")) setOpen(false); });
    document.addEventListener("keydown", function (e) { if (e.key === "Escape") setOpen(false); });
  });

  /* ── Tabs (WAI-ARIA tabs pattern) ──────────────────── */
  function selectTab(tab, focus) {
    var list = tab.closest("[role=tablist]");
    list.querySelectorAll("[role=tab]").forEach(function (t) {
      var on = t === tab;
      t.setAttribute("aria-selected", on ? "true" : "false");
      t.tabIndex = on ? 0 : -1;
      var panel = document.getElementById(t.getAttribute("aria-controls"));
      if (panel) panel.hidden = !on;
    });
    if (focus) tab.focus();
  }
  function detectOS() {
    var ua = navigator.userAgent || "";
    if (/Windows/i.test(ua)) return "windows";
    if (/Mac|iPhone|iPad|iPod/i.test(ua)) return "macos";
    return "linux";
  }
  document.querySelectorAll("[role=tablist]").forEach(function (list) {
    var tabs = Array.prototype.slice.call(list.querySelectorAll("[role=tab]"));
    tabs.forEach(function (tab, i) {
      tab.addEventListener("click", function () { selectTab(tab, false); });
      tab.addEventListener("keydown", function (e) {
        var j = null;
        if (e.key === "ArrowRight") j = (i + 1) % tabs.length;
        else if (e.key === "ArrowLeft") j = (i - 1 + tabs.length) % tabs.length;
        else if (e.key === "Home") j = 0;
        else if (e.key === "End") j = tabs.length - 1;
        if (j !== null) { e.preventDefault(); selectTab(tabs[j], true); }
      });
    });
    if (list.hasAttribute("data-os-tabs")) {
      var os = detectOS();
      var match = tabs.filter(function (t) { return t.getAttribute("data-os") === os; })[0];
      if (match) selectTab(match, false);
    }
  });

  /* ── Copy buttons ──────────────────────────────────── */
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
    return new Promise(function (resolve, reject) {
      var ta = document.createElement("textarea");
      ta.value = text; ta.setAttribute("readonly", ""); ta.className = "visually-hidden";
      document.body.appendChild(ta); ta.select();
      try { document.execCommand("copy") ? resolve() : reject(new Error("copy failed")); }
      catch (err) { reject(err); }
      finally { document.body.removeChild(ta); }
    });
  }
  document.querySelectorAll("[data-copy]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var text = btn.getAttribute("data-copy");
      if (!text) {
        var host = btn.closest(".code, .cmd");
        var src = host && host.querySelector("pre, code");
        text = src ? src.innerText.replace(/\n$/, "") : "";
      }
      var label = btn.querySelector(".copy-label");
      copyText(text).then(function () {
        btn.classList.add("is-copied");
        if (label) label.textContent = "Copied";
        setTimeout(function () {
          btn.classList.remove("is-copied");
          if (label) label.textContent = "Copy";
        }, 1600);
      }).catch(function () { if (label) label.textContent = "Press Ctrl+C"; });
    });
  });

  /* ── Docs: sidebar toggle, filter, scroll spy, anchors ─ */
  var sidebar = document.querySelector("[data-docs-sidebar]");
  if (sidebar) {
    var toggle = document.querySelector("[data-docs-toggle]");
    if (toggle) {
      toggle.addEventListener("click", function () {
        var open = !sidebar.classList.contains("is-open");
        sidebar.classList.toggle("is-open", open);
        toggle.setAttribute("aria-expanded", open ? "true" : "false");
      });
      sidebar.addEventListener("click", function (e) {
        if (e.target.closest("a")) { sidebar.classList.remove("is-open"); toggle.setAttribute("aria-expanded", "false"); }
      });
    }

    var input = sidebar.querySelector("[data-docs-filter]");
    var empty = sidebar.querySelector(".docs-nav-empty");
    if (input) {
      input.addEventListener("input", function () {
        var q = input.value.trim().toLowerCase();
        var any = false;
        sidebar.querySelectorAll(".docs-nav-group").forEach(function (group) {
          var shown = 0;
          group.querySelectorAll("li").forEach(function (li) {
            var a = li.querySelector("a");
            var hay = (a.textContent + " " + (a.getAttribute("data-keywords") || "")).toLowerCase();
            var hit = !q || hay.indexOf(q) !== -1;
            li.hidden = !hit;
            if (hit) shown++;
          });
          group.hidden = shown === 0;
          if (shown) any = true;
        });
        if (empty) empty.style.display = any ? "none" : "block";
      });
    }

    var links = Array.prototype.slice.call(sidebar.querySelectorAll('a[href^="#"]'));
    var byId = {};
    links.forEach(function (a) { byId[a.getAttribute("href").slice(1)] = a; });
    if ("IntersectionObserver" in window) {
      var obs = new IntersectionObserver(function (entries) {
        entries.forEach(function (entry) {
          if (!entry.isIntersecting) return;
          var a = byId[entry.target.id];
          if (!a) return;
          links.forEach(function (l) { l.classList.remove("is-active"); l.removeAttribute("aria-current"); });
          a.classList.add("is-active");
          a.setAttribute("aria-current", "location");
        });
      }, { rootMargin: "-20% 0px -70% 0px" });
      document.querySelectorAll(".docs-main > section[id]").forEach(function (s) { obs.observe(s); });
    }
  }

  /* Horizontally scrollable code must be reachable by keyboard (WCAG 2.1.1):
     make overflowing blocks focusable so arrow keys can scroll them. */
  function markScrollable() {
    document.querySelectorAll(".code pre, .cmd code, .window pre, .table-wrap").forEach(function (el) {
      if (el.scrollWidth > el.clientWidth + 1) {
        if (!el.hasAttribute("tabindex")) { el.setAttribute("tabindex", "0"); el.setAttribute("data-scroll-focus", ""); }
      } else if (el.hasAttribute("data-scroll-focus")) {
        el.removeAttribute("tabindex"); el.removeAttribute("data-scroll-focus");
      }
    });
  }
  markScrollable();
  var resizeTimer;
  window.addEventListener("resize", function () { clearTimeout(resizeTimer); resizeTimer = setTimeout(markScrollable, 150); });
  document.addEventListener("click", function (e) { if (e.target.closest("[role=tab]")) setTimeout(markScrollable, 0); });

  /* Self-links on headings that have an id (docs and long pages). */
  document.querySelectorAll(".prose h2[id], .prose h3[id], .docs-main section[id] > h2").forEach(function (h) {
    var id = h.id || (h.parentElement && h.parentElement.id);
    if (!id || h.querySelector(".heading-anchor")) return;
    var a = document.createElement("a");
    a.className = "heading-anchor";
    a.href = "#" + id;
    a.setAttribute("aria-label", "Link to this section");
    a.textContent = "#";
    h.appendChild(a);
  });
})();
