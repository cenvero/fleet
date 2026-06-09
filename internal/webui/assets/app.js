// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh
//
// Premium dual-pane front-end for the Cenvero Fleet web file manager. Talks to
// the localhost controller over the token-gated /api endpoints. No external
// dependencies; all behaviour lives here (CSP forbids inline scripts/handlers).

"use strict";

// ----------------------------------------------------------------- token / api

// The per-process token arrives in the URL as ?t=<token>. Read it once at load,
// preferring the URL but falling back to sessionStorage so a browser refresh
// (which strips ?t= from the address bar, below) keeps working. Persist it so
// every later request — and every reload of this tab — reuses the same value.
const TOKEN =
  new URLSearchParams(location.search).get("t") ||
  sessionStorage.getItem("fleet_token") ||
  "";
if (TOKEN) sessionStorage.setItem("fleet_token", TOKEN);

// Drop the token from the address bar so it doesn't linger in history.
if (TOKEN && window.history && window.history.replaceState) {
  window.history.replaceState(null, "", location.pathname);
}

function api(pathname, params = {}) {
  const url = new URL(pathname, location.origin);
  url.searchParams.set("t", TOKEN);
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== "") url.searchParams.set(k, v);
  }
  return url;
}

async function getJSON(pathname, params) {
  const res = await fetch(api(pathname, params), { headers: { "X-Fleet-Token": TOKEN } });
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.json();
}

async function postJSON(pathname, params) {
  const res = await fetch(api(pathname, params), { method: "POST", headers: { "X-Fleet-Token": TOKEN } });
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.json();
}

// ----------------------------------------------------------------- utilities

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

function humanSize(n) {
  if (n === undefined || n === null) return "";
  if (n < 1024) return n + " B";
  const units = ["KB", "MB", "GB", "TB", "PB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 100 ? 0 : 1) + " " + units[i];
}

function fmtTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime()) || d.getFullYear() < 1971) return "";
  const now = new Date();
  const sameYear = d.getFullYear() === now.getFullYear();
  const mon = d.toLocaleString("en-US", { month: "short" });
  const day = String(d.getDate()).padStart(2, " ");
  if (sameYear) {
    const hh = String(d.getHours()).padStart(2, "0");
    const mm = String(d.getMinutes()).padStart(2, "0");
    return `${mon} ${day}, ${hh}:${mm}`;
  }
  return `${mon} ${day}, ${d.getFullYear()}`;
}

function joinPath(dir, name) {
  if (dir.endsWith("/")) return dir + name;
  return dir + "/" + name;
}

function parentPath(p) {
  if (p === "/" || p === "") return "/";
  const trimmed = p.replace(/\/+$/, "");
  const idx = trimmed.lastIndexOf("/");
  return idx <= 0 ? "/" : trimmed.slice(0, idx);
}

function baseName(p) {
  const t = p.replace(/\/+$/, "");
  const idx = t.lastIndexOf("/");
  return idx < 0 ? t : t.slice(idx + 1);
}

// isArchiveFile reports whether a name looks like a supported archive, so the
// context menu can offer Extract on it.
function isArchiveFile(name) {
  const n = name.toLowerCase();
  return [".zip", ".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tar.xz"].some((e) => n.endsWith(e));
}

// Default archive base name from a selection: the single item's stem, else
// "archive". The format suffix is appended by the dialog.
function defaultArchiveBase(items) {
  if (items.length === 1) {
    const b = items[0].name;
    const dot = b.lastIndexOf(".");
    const stem = dot > 0 ? b.slice(0, dot) : b;
    return stem || "archive";
  }
  return "archive";
}

// ----------------------------------------------------------------- icons (inline SVG)

const ICONS = {
  folder: '<svg viewBox="0 0 24 24"><path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2V7z"/></svg>',
  file: '<svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 00-2 2v14a2 2 0 002 2h10a2 2 0 002-2V8l-5-5z"/><path d="M14 3v5h5"/></svg>',
  link: '<svg viewBox="0 0 24 24"><path d="M10 13a5 5 0 007.07 0l1.93-1.93a5 5 0 00-7.07-7.07l-1.1 1.1"/><path d="M14 11a5 5 0 00-7.07 0L5 12.93a5 5 0 007.07 7.07l1.1-1.1"/></svg>',
  download: '<svg viewBox="0 0 24 24"><path d="M12 4v12m0 0l-5-5m5 5l5-5"/><path d="M4 20h16"/></svg>',
  copy: '<svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 012-2h8"/></svg>',
  move: '<svg viewBox="0 0 24 24"><path d="M5 12h14m0 0l-5-5m5 5l-5 5"/></svg>',
  rename: '<svg viewBox="0 0 24 24"><path d="M11 4H7a2 2 0 00-2 2v12a2 2 0 002 2h12a2 2 0 002-2v-4"/><path d="M18.5 2.5a2.12 2.12 0 013 3L12 15l-4 1 1-4z"/></svg>',
  trash: '<svg viewBox="0 0 24 24"><path d="M4 7h16M9 7V5a1 1 0 011-1h4a1 1 0 011 1v2m-9 0l1 12a1 1 0 001 1h6a1 1 0 001-1l1-12"/></svg>',
  newfolder: '<svg viewBox="0 0 24 24"><path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2V7z"/><path d="M12 11v4m-2-2h4"/></svg>',
  open: '<svg viewBox="0 0 24 24"><path d="M5 12h14m0 0l-6-6m6 6l-6 6"/></svg>',
  info: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 11v5m0-8h.01"/></svg>',
  edit: '<svg viewBox="0 0 24 24"><path d="M11 4H7a2 2 0 00-2 2v12a2 2 0 002 2h12a2 2 0 002-2v-4"/><path d="M18.5 2.5a2.12 2.12 0 013 3L12 15l-4 1 1-4z"/></svg>',
  newfile: '<svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 00-2 2v14a2 2 0 002 2h10a2 2 0 002-2V8l-5-5z"/><path d="M14 3v5h5"/><path d="M12 12v5m-2.5-2.5h5"/></svg>',
  path: '<svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 012-2h8"/></svg>',
  empty: '<svg viewBox="0 0 24 24"><path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2V7z"/></svg>',
  archive: '<svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18M10 4v5m4-5v5"/><path d="M11 13h2"/></svg>',
  extract: '<svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18"/><path d="M12 12v6m0 0l-2.5-2.5M12 18l2.5-2.5"/></svg>',
  perms: '<svg viewBox="0 0 24 24"><rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 018 0v3"/></svg>',
  checksum: '<svg viewBox="0 0 24 24"><path d="M9 9l6 6m0-6l-6 6"/><circle cx="12" cy="12" r="9"/></svg>',
  duplicate: '<svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 012-2h8"/><path d="M14.5 12v5m-2.5-2.5h5"/></svg>',
};

// ----------------------------------------------------------------- state

// Panes are dynamic (1..N). Each pane carries its own source, path, selection,
// view, hidden-toggle, filter text and sort spec. `state.els` holds the matching
// DOM refs, kept index-aligned with `state.panes` across add/remove/render.
function newPaneState(server) {
  return {
    server: server || "",
    path: "/",
    items: [],            // raw listing from the server (already dir-first sorted base)
    sel: new Set(),
    hidden: false,
    loading: false,
    view: "list",
    filter: "",           // case-insensitive name filter
    sort: { key: "name", dir: 1 }, // key: name|size|mod, dir: 1 asc / -1 desc
  };
}

const MAX_PANES = 6;

const state = {
  servers: [],
  panes: [newPaneState(""), newPaneState("")],
  active: 0,
  els: [], // per-pane DOM refs, index-aligned with panes
};

function pane(i) { return state.panes[i]; }
// other(i): the "next" pane for the two-button Copy→/Move→ actions. With N panes
// it targets the pane immediately to the right (wrapping), so the toolbar arrows
// stay meaningful; drag-and-drop still works between any two panes.
function other(i) {
  if (state.panes.length < 2) return i;
  return (i + 1) % state.panes.length;
}

// ----------------------------------------------------------------- toast

function toast(message, kind = "") {
  const t = $("#toast");
  t.textContent = message;
  t.className = "toast" + (kind ? " " + kind : "");
  t.hidden = false;
  requestAnimationFrame(() => t.classList.add("show"));
  clearTimeout(toast._timer);
  toast._timer = setTimeout(() => {
    t.classList.remove("show");
    setTimeout(() => { t.hidden = true; }, 220);
  }, 3600);
}

// ----------------------------------------------------------------- pane build

// buildPanes renders all panes from scratch. Called on init and whenever the
// pane set changes (add/remove). It rebuilds DOM + refs index-aligned with
// state.panes, re-wires events, repopulates each server dropdown, and reloads.
function buildPanes() {
  const host = $("#panes");
  host.innerHTML = "";
  state.els = [];
  const tpl = $("#pane-template");
  for (let i = 0; i < state.panes.length; i++) {
    const node = tpl.content.firstElementChild.cloneNode(true);
    host.appendChild(node);
    const refs = {
      root: node,
      server: $(".pane-server", node),
      back: $(".tool.back", node),
      refresh: $(".tool.refresh", node),
      mkdir: $(".tool.mkdir", node),
      newfile: $(".tool.newfile", node),
      upload: $(".tool.upload", node),
      hiddenToggle: $(".tool.hidden-toggle", node),
      viewToggle: $(".tool.view-toggle", node),
      paneClose: $(".tool.pane-close", node),
      filterInput: $(".filter-input", node),
      filterClear: $(".filter-clear", node),
      fileInput: $(".file-input", node),
      crumbs: $(".crumbs", node),
      listHead: $(".list-head", node),
      entries: $(".entries", node),
      listing: $(".listing", node),
      body: $(".pane-body", node),
      selInfo: $(".sel-info", node),
      actSelAll: $(".act-selall", node),
      actDownload: $(".act-download", node),
      actCopy: $(".act-copy", node),
      actMove: $(".act-move", node),
      actCompress: $(".act-compress", node),
      actRename: $(".act-rename", node),
      actDelete: $(".act-delete", node),
    };
    state.els.push(refs);
    populateServerSelect(i);
    wirePane(i);
    applyViewClass(i);
    renderSort(i);
  }
  updatePaneChrome();
  setActive(Math.min(state.active, state.panes.length - 1));
  applyPaneCountClass();
}

// populateServerSelect fills one pane's source dropdown from state.servers,
// preserving the pane's current selection.
function populateServerSelect(i) {
  const sel = state.els[i].server;
  const current = pane(i).server;
  sel.innerHTML = "";
  const localOpt = document.createElement("option");
  localOpt.value = "";
  localOpt.textContent = "Local (this machine)";
  sel.appendChild(localOpt);
  for (const s of state.servers) {
    const opt = document.createElement("option");
    opt.value = s.name;
    opt.textContent = s.reachable ? s.name : s.name + " (offline)";
    opt.disabled = !s.reachable;
    sel.appendChild(opt);
  }
  sel.value = current;
}

// applyPaneCountClass tags #panes with the live count so CSS can pick a layout
// (and switch to horizontal-scroll once panes get too narrow to fit).
function applyPaneCountClass() {
  const host = $("#panes");
  host.dataset.count = String(state.panes.length);
}

// updatePaneChrome enables/disables the close button (min 1 pane) and the global
// Add-pane button (max MAX_PANES), and keeps each pane's title in sync.
function updatePaneChrome() {
  const single = state.panes.length <= 1;
  for (let i = 0; i < state.els.length; i++) {
    if (state.els[i].paneClose) state.els[i].paneClose.disabled = single;
  }
  const addBtn = $("#add-pane");
  if (addBtn) addBtn.disabled = state.panes.length >= MAX_PANES;
}

// addPane appends a new pane (default source: first reachable server, else
// Local), rebuilds, and loads it. Selection/active state of others is preserved.
function addPane() {
  if (state.panes.length >= MAX_PANES) {
    toast("Maximum of " + MAX_PANES + " panes", "error");
    return;
  }
  const reachable = state.servers.filter((s) => s.reachable);
  const defServer = reachable[0] ? reachable[0].name : "";
  state.panes.push(newPaneState(defServer));
  const idx = state.panes.length - 1;
  buildPanes();
  setActive(idx);
  loadListing(idx);
}

// removePane drops pane i (keeping at least one), rebuilds, and reloads the
// remaining panes so their refs/listings stay valid.
function removePane(i) {
  if (state.panes.length <= 1) return;
  state.panes.splice(i, 1);
  if (state.active >= state.panes.length) state.active = state.panes.length - 1;
  buildPanes();
  for (let k = 0; k < state.panes.length; k++) loadListing(k);
}

function wirePane(i) {
  const r = state.els[i];
  const p = pane(i);

  r.root.addEventListener("pointerdown", () => setActive(i), true);
  r.root.addEventListener("focusin", () => setActive(i));

  r.server.addEventListener("change", (e) => {
    setActive(i);
    p.server = e.target.value;
    p.path = "/";
    p.sel.clear();
    loadListing(i);
  });

  r.back.addEventListener("click", () => { setActive(i); navigate(i, parentPath(p.path)); });
  r.refresh.addEventListener("click", () => { setActive(i); loadListing(i); });
  r.mkdir.addEventListener("click", () => { setActive(i); promptMkdir(i); });
  r.newfile.addEventListener("click", () => { setActive(i); promptNewFile(i); });
  r.upload.addEventListener("click", () => { setActive(i); r.fileInput.click(); });
  r.paneClose.addEventListener("click", (e) => { e.stopPropagation(); removePane(i); });
  r.fileInput.addEventListener("change", (e) => {
    if (e.target.files.length) uploadFiles(i, e.target.files);
    e.target.value = "";
  });
  r.hiddenToggle.addEventListener("click", () => {
    setActive(i);
    p.hidden = !p.hidden;
    r.hiddenToggle.setAttribute("aria-pressed", String(p.hidden));
    loadListing(i);
  });
  r.viewToggle.addEventListener("click", () => {
    setActive(i);
    setView(i, p.view === "list" ? "icons" : "list");
  });

  // per-pane filter box
  r.filterInput.value = p.filter;
  r.filterClear.hidden = !p.filter;
  r.filterInput.addEventListener("input", (e) => {
    setActive(i);
    p.filter = e.target.value;
    r.filterClear.hidden = !p.filter;
    renderEntries(i);
    renderSelection(i);
  });
  r.filterClear.addEventListener("click", () => {
    p.filter = "";
    r.filterInput.value = "";
    r.filterClear.hidden = true;
    r.filterInput.focus();
    renderEntries(i);
    renderSelection(i);
  });

  // sortable column headers
  for (const col of $$(".list-head .col", r.root)) {
    col.addEventListener("click", () => {
      setActive(i);
      const key = col.dataset.sort;
      if (p.sort.key === key) p.sort.dir = -p.sort.dir;
      else { p.sort.key = key; p.sort.dir = key === "name" ? 1 : -1; }
      renderSort(i);
      renderEntries(i);
      renderSelection(i);
    });
  }

  r.actSelAll.addEventListener("click", () => { setActive(i); selectAll(i); });
  r.actDownload.addEventListener("click", () => actDownload(i));
  r.actCopy.addEventListener("click", () => transferSelection(i, "copy"));
  r.actMove.addEventListener("click", () => transferSelection(i, "move"));
  r.actCompress.addEventListener("click", () => actCompress(i));
  r.actRename.addEventListener("click", () => actRename(i));
  r.actDelete.addEventListener("click", () => actDelete(i));

  // background click clears selection
  r.listing.addEventListener("click", (e) => {
    if (e.target === r.listing || e.target === r.entries) {
      p.sel.clear();
      renderSelection(i);
    }
  });
  r.listing.addEventListener("contextmenu", (e) => {
    if (e.target === r.listing || e.target === r.entries) {
      e.preventDefault();
      setActive(i);
      openContextMenu(i, null, e.clientX, e.clientY);
    }
  });

  // desktop file drag → upload (native HTML5 file DnD)
  setupFileDropZone(i);
}

function setActive(i) {
  state.active = i;
  for (let k = 0; k < state.els.length; k++) {
    state.els[k].root.classList.toggle("active", k === i);
  }
}

// renderSort reflects the pane's current sort onto its column headers (which
// column is active + the asc/desc arrow direction).
function renderSort(i) {
  const p = pane(i);
  const r = state.els[i];
  for (const col of $$(".list-head .col", r.root)) {
    const active = col.dataset.sort === p.sort.key;
    col.classList.toggle("sort-active", active);
    col.classList.toggle("sort-desc", active && p.sort.dir < 0);
    col.classList.toggle("sort-asc", active && p.sort.dir > 0);
  }
}

// ----------------------------------------------------------------- servers

async function loadServers() {
  let servers = [];
  try {
    servers = await getJSON("/api/servers");
  } catch (e) {
    toast("Failed to load servers: " + e.message, "error");
    return;
  }
  state.servers = servers;
  // Default panes: pane 0 = Local, pane 1 = first reachable server (or Local
  // when no servers are configured / reachable). Only seed defaults the very
  // first time; later panes keep whatever the user picked.
  const reachable = servers.filter((s) => s.reachable);
  const firstServer = reachable[0] || servers[0];
  pane(0).server = "";
  if (state.panes.length > 1) pane(1).server = firstServer ? firstServer.name : "";
  // Build each pane's source dropdown ("Local" + every configured server).
  for (let i = 0; i < state.panes.length; i++) populateServerSelect(i);
  await Promise.all(state.panes.map((_, i) => loadListing(i)));
}

// ----------------------------------------------------------------- listing

async function loadListing(i) {
  const p = pane(i);
  const r = state.els[i];
  p.loading = true;
  showPlaceholder(i, "spinner", "Loading…");
  let result;
  try {
    // server "" means the Local (controller) filesystem; it's still sent so the
    // backend can route, and our api() helper just drops the empty value.
    const params = { server: p.server, path: p.path };
    if (p.hidden) params.hidden = "1";
    result = await getJSON("/api/list", params);
  } catch (e) {
    p.loading = false;
    showPlaceholder(i, "error", "Error: " + e.message);
    renderCrumbs(i);
    return;
  }
  p.loading = false;
  p.path = result.path || p.path;
  // keep selections that still exist
  const names = new Set((result.entries || []).map((x) => x.name));
  for (const n of Array.from(p.sel)) if (!names.has(n)) p.sel.delete(n);
  p.items = (result.entries || []).slice();
  renderCrumbs(i);
  renderEntries(i);
  renderSelection(i);
}

// sortItems returns p.items ordered by the pane's sort spec. Directories always
// sort before files; within each group the chosen key (name/size/mod) applies,
// reversed for descending. Name is the stable tiebreaker.
function sortItems(p) {
  const { key, dir } = p.sort;
  const byName = (a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
  return p.items.slice().sort((a, b) => {
    if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
    let c = 0;
    if (key === "size") c = (a.is_dir ? 0 : a.size || 0) - (b.is_dir ? 0 : b.size || 0);
    else if (key === "mod") c = new Date(a.mod_time || 0) - new Date(b.mod_time || 0);
    else c = byName(a, b);
    if (c === 0) c = byName(a, b);
    return c * dir;
  });
}

// visibleItems applies the pane's sort then its case-insensitive name filter.
function visibleItems(p) {
  let items = sortItems(p);
  const f = p.filter.trim().toLowerCase();
  if (f) items = items.filter((it) => it.name.toLowerCase().includes(f));
  return items;
}

function navigate(i, path) {
  const p = pane(i);
  if (p.path === path) return;
  p.path = path;
  p.sel.clear();
  loadListing(i);
}

function showPlaceholder(i, kind, text) {
  const r = state.els[i];
  r.entries.innerHTML = "";
  const ph = document.createElement("div");
  ph.className = "placeholder" + (kind === "error" ? " error" : "");
  if (kind === "spinner") {
    const sp = document.createElement("div");
    sp.className = "spinner";
    ph.appendChild(sp);
  } else {
    const wrap = document.createElement("div");
    wrap.innerHTML = ICONS.empty;
    const svg = wrap.firstElementChild;
    svg.classList.add("ph-ico");
    ph.appendChild(svg);
  }
  const span = document.createElement("div");
  span.textContent = text;
  ph.appendChild(span);
  r.entries.appendChild(ph);
}

function iconFor(item) {
  if (item.is_dir) return ICONS.folder;
  if (item.is_symlink) return ICONS.link;
  return ICONS.file;
}

function renderCrumbs(i) {
  const p = pane(i);
  const c = state.els[i].crumbs;
  c.innerHTML = "";
  const parts = p.path.split("/").filter(Boolean);
  const rootBtn = document.createElement("button");
  rootBtn.className = "crumb" + (parts.length === 0 ? " current" : "");
  rootBtn.textContent = "/";
  rootBtn.dataset.path = "/";
  if (parts.length) rootBtn.addEventListener("click", () => navigate(i, "/"));
  c.appendChild(rootBtn);

  let acc = "";
  parts.forEach((seg, idx) => {
    acc += "/" + seg;
    const sep = document.createElement("span");
    sep.className = "crumb-sep";
    sep.textContent = "›";
    c.appendChild(sep);
    const b = document.createElement("button");
    const last = idx === parts.length - 1;
    b.className = "crumb" + (last ? " current" : "");
    b.textContent = seg;
    const target = acc;
    b.dataset.path = target;
    if (!last) b.addEventListener("click", () => navigate(i, target));
    c.appendChild(b);
  });
}

function renderEntries(i) {
  const p = pane(i);
  const r = state.els[i];
  r.entries.innerHTML = "";
  if (!p.items.length) {
    showPlaceholder(i, "empty", "Empty folder");
    return;
  }
  const items = visibleItems(p);
  if (!items.length) {
    showPlaceholder(i, "empty", "No matches for “" + p.filter + "”");
    return;
  }
  const frag = document.createDocumentFragment();
  const icons = p.view === "icons";
  for (const item of items) {
    const row = icons ? buildIconCell(item) : buildListRow(item);
    wireRow(i, row, item);
    frag.appendChild(row);
  }
  r.entries.appendChild(frag);
}

// buildListRow builds a Finder-style "List" row: icon + name, size, modified.
function buildListRow(item) {
  const row = document.createElement("div");
  row.className = "row " + (item.is_dir ? "dir" : item.is_symlink ? "link" : "file");
  row.dataset.name = item.name;

  const nameCell = document.createElement("div");
  nameCell.className = "row-name";
  const icon = document.createElement("span");
  icon.className = "row-icon";
  icon.innerHTML = iconFor(item);
  const label = document.createElement("span");
  label.className = "row-label";
  label.textContent = item.name;
  nameCell.append(icon, label);

  const size = document.createElement("span");
  size.className = "row-size";
  size.textContent = item.is_dir ? "—" : humanSize(item.size);

  const mod = document.createElement("span");
  mod.className = "row-mod";
  mod.textContent = fmtTime(item.mod_time);

  row.append(nameCell, size, mod);
  return row;
}

// buildIconCell builds an "Icons" grid cell: a large icon over the name. It
// keeps the same `.row` + `.dir/.file/.link` classes and `data-name` as a list
// row so selection, drag-and-drop, and the context menu work identically.
function buildIconCell(item) {
  const cell = document.createElement("div");
  cell.className = "row cell " + (item.is_dir ? "dir" : item.is_symlink ? "link" : "file");
  cell.dataset.name = item.name;

  const icon = document.createElement("span");
  icon.className = "cell-icon";
  icon.innerHTML = iconFor(item);

  const label = document.createElement("span");
  label.className = "cell-label";
  label.textContent = item.name;
  label.title = item.name;

  cell.append(icon, label);
  return cell;
}

// setView switches a pane between "list" and "icons" and re-renders in place.
// Nothing is persisted — the view resets to List on reload.
function setView(i, view) {
  const p = pane(i);
  if (p.view === view) return;
  p.view = view;
  applyViewClass(i);
  renderEntries(i);
  renderSelection(i);
}

// applyViewClass reflects the pane's view onto its DOM and updates the toggle
// button's title/state so the icon swap (grid vs list glyph) is meaningful.
function applyViewClass(i) {
  const p = pane(i);
  const r = state.els[i];
  const icons = p.view === "icons";
  r.root.classList.toggle("view-icons", icons);
  r.root.classList.toggle("view-list", !icons);
  if (r.viewToggle) {
    r.viewToggle.setAttribute("aria-pressed", String(icons));
    r.viewToggle.title = icons ? "Switch to list view" : "Switch to icon view";
  }
}

function rowEl(i, name) {
  return state.els[i].entries.querySelector(`.row[data-name="${cssEscape(name)}"]`);
}

function cssEscape(s) {
  if (window.CSS && CSS.escape) return CSS.escape(s);
  return s.replace(/["\\]/g, "\\$&");
}

// ----------------------------------------------------------------- selection

function renderSelection(i) {
  const p = pane(i);
  const r = state.els[i];
  for (const row of $$(".row", r.entries)) {
    row.classList.toggle("selected", p.sel.has(row.dataset.name));
  }
  const n = p.sel.size;
  const selItems = p.items.filter((it) => p.sel.has(it.name));
  const sz = selItems.reduce((a, it) => a + (it.is_dir ? 0 : it.size || 0), 0);
  r.selInfo.textContent = n === 0 ? "" : n === 1 ? "1 selected" : `${n} selected · ${humanSize(sz)}`;

  const onlyFiles = n > 0 && selItems.every((it) => !it.is_dir);
  r.actDownload.disabled = !onlyFiles;
  r.actCopy.disabled = n === 0;
  r.actMove.disabled = n === 0;
  r.actCompress.disabled = n === 0;
  r.actRename.disabled = n !== 1;
  r.actDelete.disabled = n === 0;

  // Select-all label toggles to "Deselect" once everything visible is selected.
  const vis = visibleItems(p);
  const allSel = vis.length > 0 && vis.every((it) => p.sel.has(it.name));
  r.actSelAll.textContent = allSel ? "Deselect all" : "Select all";
  r.actSelAll.disabled = vis.length === 0;
}

// selectAll toggles selection over the currently visible (filtered+sorted) set.
function selectAll(i) {
  const p = pane(i);
  const vis = visibleItems(p);
  const allSel = vis.length > 0 && vis.every((it) => p.sel.has(it.name));
  if (allSel) {
    for (const it of vis) p.sel.delete(it.name);
  } else {
    for (const it of vis) p.sel.add(it.name);
  }
  renderSelection(i);
}

function selectedItems(i) {
  const p = pane(i);
  return p.items.filter((it) => p.sel.has(it.name));
}

function toggleSelect(i, name, opts = {}) {
  const p = pane(i);
  if (opts.range && p.sel.size && p._anchor != null) {
    const names = visibleItems(p).map((x) => x.name);
    const a = names.indexOf(p._anchor);
    const b = names.indexOf(name);
    if (a >= 0 && b >= 0) {
      const [lo, hi] = a < b ? [a, b] : [b, a];
      if (!opts.additive) p.sel.clear();
      for (let k = lo; k <= hi; k++) p.sel.add(names[k]);
      renderSelection(i);
      return;
    }
  }
  if (opts.additive) {
    if (p.sel.has(name)) p.sel.delete(name);
    else p.sel.add(name);
  } else {
    p.sel.clear();
    p.sel.add(name);
  }
  p._anchor = name;
  renderSelection(i);
}

// ----------------------------------------------------------------- row wiring

function wireRow(i, row, item) {
  const p = pane(i);

  row.addEventListener("click", (e) => {
    setActive(i);
    toggleSelect(i, item.name, { additive: e.metaKey || e.ctrlKey, range: e.shiftKey });
  });

  row.addEventListener("dblclick", () => {
    setActive(i);
    if (item.is_dir) {
      navigate(i, joinPath(p.path, item.name));
    } else if (isTextFile(item.name)) {
      openEditor(i, item);
    } else {
      downloadOne(i, item);
    }
  });

  row.addEventListener("contextmenu", (e) => {
    e.preventDefault();
    setActive(i);
    if (!p.sel.has(item.name)) toggleSelect(i, item.name, {});
    openContextMenu(i, item, e.clientX, e.clientY);
  });

  // pointer-driven drag
  row.addEventListener("pointerdown", (e) => onRowPointerDown(i, row, item, e));
}

// ----------------------------------------------------------------- download

function downloadOne(i, item) {
  const p = pane(i);
  const url = api("/api/download", { server: p.server, path: joinPath(p.path, item.name) });
  const a = document.createElement("a");
  a.href = url.toString();
  a.download = item.name;
  document.body.appendChild(a);
  a.click();
  a.remove();
}

function actDownload(i) {
  const files = selectedItems(i).filter((it) => !it.is_dir);
  if (!files.length) { toast("Select a file to download", "error"); return; }
  files.forEach((f, idx) => setTimeout(() => downloadOne(i, f), idx * 250));
}

// ----------------------------------------------------------------- mkdir / rename / delete

function promptMkdir(i) {
  openInputModal({
    title: "New folder",
    desc: "Create a folder in " + pane(i).path,
    value: "untitled folder",
    okLabel: "Create",
    onOk: async (name) => {
      const p = pane(i);
      try {
        await postJSON("/api/mkdir", { server: p.server, path: joinPath(p.path, name) });
        toast("Created " + name, "success");
        loadListing(i);
      } catch (e) {
        toast("New folder failed: " + e.message, "error");
      }
    },
  });
}

function promptNewFile(i) {
  openInputModal({
    title: "New file",
    desc: "Create an empty file in " + pane(i).path,
    value: "untitled.txt",
    okLabel: "Create",
    onOk: async (name) => {
      const p = pane(i);
      try {
        await postJSON("/api/touch", { server: p.server, path: joinPath(p.path, name) });
        toast("Created " + name, "success");
        await loadListing(i);
        // Open the new file straight in the editor for convenience.
        openEditor(i, { name, is_dir: false });
      } catch (e) {
        toast("New file failed: " + e.message, "error");
      }
    },
  });
}

function actRename(i) {
  const sel = selectedItems(i);
  if (sel.length !== 1) return;
  const item = sel[0];
  openInputModal({
    title: "Rename",
    desc: "Rename “" + item.name + "”",
    value: item.name,
    okLabel: "Rename",
    onOk: async (name) => {
      if (name === item.name) return;
      const p = pane(i);
      try {
        await postJSON("/api/mv", { server: p.server, from: joinPath(p.path, item.name), to: joinPath(p.path, name) });
        toast("Renamed to " + name, "success");
        p.sel.clear();
        loadListing(i);
      } catch (e) {
        toast("Rename failed: " + e.message, "error");
      }
    },
  });
}

function actDelete(i) {
  const sel = selectedItems(i);
  if (!sel.length) return;
  const hasDir = sel.some((it) => it.is_dir);
  const label = sel.length === 1 ? "“" + sel[0].name + "”" : sel.length + " items";
  openConfirmModal({
    title: "Delete " + (sel.length === 1 ? "item" : "items"),
    desc: "Permanently delete " + label + (hasDir ? " and all contents" : "") + "? This cannot be undone.",
    okLabel: "Delete",
    danger: true,
    onOk: async () => {
      const p = pane(i);
      let ok = 0;
      for (const item of sel) {
        try {
          await postJSON("/api/rm", {
            server: p.server,
            path: joinPath(p.path, item.name),
            recursive: item.is_dir ? "true" : "false",
          });
          ok++;
        } catch (e) {
          toast("Delete failed (" + item.name + "): " + e.message, "error");
        }
      }
      if (ok) toast("Deleted " + ok + (ok === 1 ? " item" : " items"), "success");
      p.sel.clear();
      loadListing(i);
    },
  });
}

// ----------------------------------------------------------------- transfers (copy / move across panes)

// transferSelection: from pane `i` to the other pane's current dir.
async function transferSelection(srcPaneIdx, kind) {
  const items = selectedItems(srcPaneIdx);
  if (!items.length) return;
  const dstIdx = other(srcPaneIdx);
  await runTransfer(srcPaneIdx, items, dstIdx, pane(dstIdx).path, kind);
}

// runTransfer: copy/move `items` from src pane into dstDir on dst pane.
async function runTransfer(srcIdx, items, dstIdx, dstDir, kind) {
  const sp = pane(srcIdx);
  const dp = pane(dstIdx);
  const hasDir = items.some((it) => it.is_dir);

  // same-server + same dir guard
  if (sp.server === dp.server && sp.path === dstDir) {
    toast("Source and destination are the same folder", "error");
    return;
  }

  if (hasDir) {
    const label = items.length === 1 ? "“" + items[0].name + "”" : items.length + " items";
    const verb = kind === "move" ? "Move" : "Copy";
    const proceed = await confirmAsync({
      title: verb + " folder",
      desc: verb + " " + label + " and all its contents to " + dstDir + "?",
      okLabel: verb,
    });
    if (!proceed) return;
  }

  for (const item of items) {
    const srcPath = joinPath(sp.path, item.name);
    const dstPath = joinPath(dstDir, item.name);
    const recursive = item.is_dir;
    const endpoint = kind === "move" ? "/api/move" : "/api/copy";
    let resp;
    try {
      resp = await postJSON(endpoint, {
        srcServer: sp.server,
        srcPath,
        dstServer: dp.server,
        dstPath,
        recursive: recursive ? "1" : "",
      });
    } catch (e) {
      toast((kind === "move" ? "Move" : "Copy") + " failed: " + e.message, "error");
      continue;
    }
    const verb = kind === "move" ? "Move" : "Copy";
    watchTransfer(resp.id, verb + " " + item.name, "copy", () => {
      loadListing(dstIdx);
      if (kind === "move") loadListing(srcIdx);
    });
  }
  if (kind === "move") sp.sel.clear();
}

// same-pane move (rename within a server) — drop onto a folder in same pane.
async function moveIntoFolder(i, items, destName) {
  const p = pane(i);
  const destPath = joinPath(p.path, destName);
  let moved = 0;
  for (const item of items) {
    if (item.name === destName) continue;
    try {
      await postJSON("/api/mv", {
        server: p.server,
        from: joinPath(p.path, item.name),
        to: joinPath(destPath, item.name),
      });
      moved++;
    } catch (e) {
      toast("Move failed (" + item.name + "): " + e.message, "error");
    }
  }
  if (moved) toast("Moved " + moved + (moved === 1 ? " item" : " items"), "success");
  p.sel.clear();
  loadListing(i);
}

// ----------------------------------------------------------------- uploads

async function uploadFiles(i, fileList) {
  const p = pane(i);
  for (const file of fileList) {
    let resp;
    try {
      const res = await fetch(api("/api/upload", { server: p.server, dir: p.path, name: file.name }), {
        method: "POST",
        headers: { "X-Fleet-Token": TOKEN },
        body: file,
      });
      if (!res.ok) throw new Error((await res.text()) || res.statusText);
      resp = await res.json();
    } catch (e) {
      toast("Upload failed (" + file.name + "): " + e.message, "error");
      continue;
    }
    // A server upload returns a progress id to track; a Local upload writes the
    // bytes straight to disk and finishes synchronously (no id, no progress).
    if (resp.id) {
      watchTransfer(resp.id, "Upload " + file.name, "upload", () => loadListing(i));
    } else {
      toast("Uploaded " + file.name, "success");
      loadListing(i);
    }
  }
}

// ----------------------------------------------------------------- transfers dock

const transfers = new Map();
let dockExpandedByUser = true;

function watchTransfer(id, label, kind, onDone) {
  expandDock();
  const card = document.createElement("div");
  card.className = "transfer";
  const ico = kind === "upload" ? ICONS.download : ICONS.copy;
  const nameWrap = document.createElement("div");
  nameWrap.className = "t-name";
  const icoSpan = document.createElement("span");
  icoSpan.innerHTML = ico;
  const icoSvg = icoSpan.firstElementChild;
  icoSvg.classList.add("t-ico");
  const txt = document.createElement("span");
  txt.className = "t-text";
  txt.textContent = label;
  nameWrap.append(icoSvg, txt);

  const pct = document.createElement("span");
  pct.className = "pct";
  pct.textContent = "0%";

  const labelRow = document.createElement("div");
  labelRow.className = "label";
  labelRow.append(nameWrap, pct);

  const bar = document.createElement("div");
  bar.className = "progress";
  const fill = document.createElement("span");
  bar.appendChild(fill);

  const meta = document.createElement("div");
  meta.className = "meta";
  meta.textContent = "starting…";

  card.append(labelRow, bar, meta);

  const list = $("#transfers");
  const empty = list.querySelector(".empty");
  if (empty) empty.remove();
  list.prepend(card);
  transfers.set(id, card);
  updateDockCount();

  const es = new EventSource(api("/api/progress", { id }).toString());
  es.onmessage = (ev) => {
    let pr;
    try { pr = JSON.parse(ev.data); } catch { return; }
    fill.style.width = (pr.percent || 0) + "%";
    pct.textContent = (pr.percent || 0) + "%";
    if (pr.error) {
      card.classList.add("error");
      meta.textContent = "Error: " + pr.error;
    } else if (pr.done) {
      card.classList.add("done");
      fill.style.width = "100%";
      pct.textContent = "100%";
      meta.textContent = "Done · " + humanSize(pr.total_bytes);
    } else {
      const rate = pr.rate_per_sec > 0 ? humanSize(pr.rate_per_sec) + "/s" : "…";
      const streams = pr.active_streams ? " · " + pr.active_streams + " streams" : "";
      meta.textContent = `${humanSize(pr.bytes_done)} / ${humanSize(pr.total_bytes)} · ${rate}${streams}`;
    }
    if (pr.done) {
      es.close();
      transfers.delete(id);
      updateDockCount();
      if (onDone) onDone();
    }
  };
  es.onerror = () => es.close();
}

function activeTransferCount() { return transfers.size; }

function updateDockCount() {
  const total = $("#transfers").querySelectorAll(".transfer").length;
  const active = activeTransferCount();
  const el = $("#dock-count");
  el.textContent = active > 0 ? String(active) : String(total);
  el.classList.toggle("idle", active === 0);
}

function expandDock() {
  if (!dockExpandedByUser) return;
  $("#dock").classList.remove("collapsed");
}

function setupDock() {
  const dock = $("#dock");
  $("#dock-toggle").addEventListener("click", () => {
    const collapsed = dock.classList.toggle("collapsed");
    dockExpandedByUser = !collapsed;
  });
  updateDockCount();
}

// ----------------------------------------------------------------- context menu

function closeMenus() {
  $("#ctxmenu").hidden = true;
  $("#ctxmenu").innerHTML = "";
  $("#dropmenu").hidden = true;
  $("#dropmenu").innerHTML = "";
}

function makeMenuItem(label, icon, handler, opts = {}) {
  const b = document.createElement("button");
  b.className = "menu-item" + (opts.danger ? " danger" : "");
  if (icon) {
    const wrap = document.createElement("span");
    wrap.innerHTML = icon;
    const svg = wrap.firstElementChild;
    svg.classList.add("m-ico");
    b.appendChild(svg);
  }
  const span = document.createElement("span");
  span.textContent = label;
  b.appendChild(span);
  if (opts.key) {
    const k = document.createElement("span");
    k.className = "m-key";
    k.textContent = opts.key;
    b.appendChild(k);
  }
  if (opts.disabled) b.disabled = true;
  else b.addEventListener("click", () => { closeMenus(); handler(); });
  return b;
}

function openContextMenu(i, item, x, y) {
  closeMenus();
  const menu = $("#ctxmenu");
  const p = pane(i);
  const sel = selectedItems(i);
  const multi = sel.length > 1;
  const oneFile = item && !item.is_dir;
  const otherIdx = other(i);

  const add = (el) => menu.appendChild(el);

  if (item) {
    if (item.is_dir) {
      add(makeMenuItem("Open", ICONS.open, () => navigate(i, joinPath(p.path, item.name))));
    } else {
      if (isTextFile(item.name)) {
        add(makeMenuItem("Edit", ICONS.edit, () => openEditor(i, item), { disabled: multi }));
      }
      add(makeMenuItem("Download", ICONS.download, () => actDownload(i)));
    }
    add(makeMenuItem("Copy → next pane", ICONS.copy, () => transferSelection(i, "copy")));
    add(makeMenuItem("Move → next pane", ICONS.move, () => transferSelection(i, "move")));
    add(makeMenuItem("Duplicate", ICONS.duplicate, () => actDuplicate(i, item), { disabled: multi }));
    add(makeMenuItem("Rename", ICONS.rename, () => actRename(i), { disabled: multi }));
    add(makeMenuItem("Copy path", ICONS.path, () => copyPath(i, item), { disabled: multi }));
    add(menuSep());
    add(makeMenuItem("Compress…", ICONS.archive, () => actCompress(i)));
    if (oneFile && isArchiveFile(item.name)) {
      add(makeMenuItem("Extract here", ICONS.extract, () => actExtract(i, item), { disabled: multi }));
    }
    add(makeMenuItem("Permissions…", ICONS.perms, () => actChmod(i, item), { disabled: multi }));
    if (oneFile) {
      add(makeMenuItem("Checksum (SHA-256)", ICONS.checksum, () => actChecksum(i, item), { disabled: multi }));
    }
    add(menuSep());
    add(makeMenuItem("Delete", ICONS.trash, () => actDelete(i), { danger: true, key: "⌫" }));
    add(menuSep());
  }
  add(makeMenuItem("New folder", ICONS.newfolder, () => promptMkdir(i)));
  add(makeMenuItem("New file", ICONS.newfile, () => promptNewFile(i)));
  add(makeMenuItem("Upload files…", ICONS.download, () => state.els[i].fileInput.click()));
  add(makeMenuItem("Select all", null, () => selectAll(i)));
  add(makeMenuItem("Refresh", null, () => loadListing(i)));
  if (item) {
    add(menuSep());
    add(makeMenuItem("Properties", ICONS.info, () => showProperties(i, item)));
  }

  positionFloating(menu, x, y);
}

function menuSep() { const d = document.createElement("div"); d.className = "menu-sep"; return d; }

function positionFloating(el, x, y) {
  el.hidden = false;
  el.style.left = "0px";
  el.style.top = "0px";
  const rect = el.getBoundingClientRect();
  let nx = x, ny = y;
  if (x + rect.width > window.innerWidth - 8) nx = window.innerWidth - rect.width - 8;
  if (y + rect.height > window.innerHeight - 8) ny = window.innerHeight - rect.height - 8;
  el.style.left = Math.max(8, nx) + "px";
  el.style.top = Math.max(8, ny) + "px";
}

// ----------------------------------------------------------------- properties modal

function showProperties(i, item) {
  const p = pane(i);
  const rows = [
    ["Name", item.name],
    ["Type", item.is_dir ? "Folder" : item.is_symlink ? "Symbolic link" : "File"],
    ["Path", joinPath(p.path, item.name)],
    ["Size", item.is_dir ? "—" : humanSize(item.size) + " (" + (item.size || 0).toLocaleString() + " bytes)"],
    ["Modified", fmtTime(item.mod_time) || "—"],
    ["Mode", item.mode != null ? "0" + (item.mode & 0o777).toString(8) : "—"],
    ["Source", p.server || "Local (this machine)"],
  ];
  const modal = openModalShell();
  const h = document.createElement("h3");
  h.textContent = "Properties";
  modal.appendChild(h);
  const props = document.createElement("div");
  props.className = "modal-props";
  for (const [k, v] of rows) {
    const r = document.createElement("div");
    r.className = "pr";
    const kk = document.createElement("span"); kk.className = "k"; kk.textContent = k;
    const vv = document.createElement("span"); vv.className = "v"; vv.textContent = v;
    r.append(kk, vv);
    props.appendChild(r);
  }
  modal.appendChild(props);
  const actions = document.createElement("div");
  actions.className = "modal-actions";
  const close = mkBtn("Close", "primary", closeModal);
  actions.appendChild(close);
  modal.appendChild(actions);
  showModal();
  close.focus();
}

// ----------------------------------------------------------------- modal infra

function openModalShell() {
  const modal = $("#modal");
  modal.innerHTML = "";
  return modal;
}
function showModal() {
  $("#modal-overlay").hidden = false;
}
function closeModal() {
  $("#modal-overlay").hidden = true;
  $("#modal").innerHTML = "";
}
function mkBtn(label, cls, handler) {
  const b = document.createElement("button");
  b.className = "mbtn" + (cls ? " " + cls : "");
  b.textContent = label;
  b.addEventListener("click", handler);
  return b;
}

function openInputModal({ title, desc, value, okLabel, onOk }) {
  const modal = openModalShell();
  const h = document.createElement("h3"); h.textContent = title; modal.appendChild(h);
  if (desc) { const d = document.createElement("p"); d.className = "modal-desc"; d.textContent = desc; modal.appendChild(d); }
  const input = document.createElement("input");
  input.type = "text";
  input.value = value || "";
  input.spellcheck = false;
  modal.appendChild(input);
  const actions = document.createElement("div"); actions.className = "modal-actions";
  const cancel = mkBtn("Cancel", "", closeModal);
  const ok = mkBtn(okLabel || "OK", "primary", submit);
  actions.append(cancel, ok);
  modal.appendChild(actions);
  function submit() {
    const v = input.value.trim();
    if (!v) { input.focus(); return; }
    closeModal();
    onOk(v);
  }
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); submit(); }
    if (e.key === "Escape") { e.preventDefault(); closeModal(); }
  });
  showModal();
  input.focus();
  // select base name (without extension) for convenience
  const dot = (value || "").lastIndexOf(".");
  if (dot > 0) input.setSelectionRange(0, dot);
  else input.select();
}

function openConfirmModal({ title, desc, okLabel, danger, onOk }) {
  const modal = openModalShell();
  const h = document.createElement("h3"); h.textContent = title; modal.appendChild(h);
  if (desc) { const d = document.createElement("p"); d.className = "modal-desc"; d.textContent = desc; modal.appendChild(d); }
  const actions = document.createElement("div"); actions.className = "modal-actions";
  const cancel = mkBtn("Cancel", "", closeModal);
  const ok = mkBtn(okLabel || "OK", danger ? "danger" : "primary", () => { closeModal(); onOk(); });
  actions.append(cancel, ok);
  modal.appendChild(actions);
  showModal();
  ok.focus();
}

// confirmAsync — promise-based confirm using the modal.
function confirmAsync({ title, desc, okLabel }) {
  return new Promise((resolve) => {
    const modal = openModalShell();
    const h = document.createElement("h3"); h.textContent = title; modal.appendChild(h);
    if (desc) { const d = document.createElement("p"); d.className = "modal-desc"; d.textContent = desc; modal.appendChild(d); }
    const actions = document.createElement("div"); actions.className = "modal-actions";
    const cancel = mkBtn("Cancel", "", () => { closeModal(); resolve(false); });
    const ok = mkBtn(okLabel || "OK", "primary", () => { closeModal(); resolve(true); });
    actions.append(cancel, ok);
    modal.appendChild(actions);
    showModal();
    ok.focus();
  });
}

// ----------------------------------------------------------------- pointer drag (ghost)

const drag = {
  active: false,
  started: false,
  srcIdx: -1,
  items: [],
  startX: 0,
  startY: 0,
  pointerId: null,
  ghost: null,
  target: null, // { kind:'pane'|'folder'|'crumb', paneIdx, name?, path?, el }
};

function onRowPointerDown(i, row, item, e) {
  if (e.button !== 0) return; // left only
  const p = pane(i);
  // Build the drag set: if item is in selection, drag whole selection; else just it.
  let items;
  if (p.sel.has(item.name)) {
    items = selectedItems(i);
  } else {
    items = [item];
  }
  drag.active = true;
  drag.started = false;
  drag.srcIdx = i;
  drag.items = items;
  drag.startX = e.clientX;
  drag.startY = e.clientY;
  drag.pointerId = e.pointerId;
  drag.row = row;

  window.addEventListener("pointermove", onDragMove);
  window.addEventListener("pointerup", onDragUp);
}

function onDragMove(e) {
  if (!drag.active) return;
  const dx = e.clientX - drag.startX;
  const dy = e.clientY - drag.startY;
  if (!drag.started) {
    if (Math.hypot(dx, dy) < 6) return;
    drag.started = true;
    startGhost();
  }
  positionGhost(e.clientX, e.clientY);
  updateDropTarget(e.clientX, e.clientY);
}

function startGhost() {
  const g = $("#ghost");
  const first = drag.items[0];
  $(".ghost-icon", g).innerHTML = iconFor(first);
  $(".ghost-name", g).textContent = drag.items.length === 1 ? first.name : drag.items.length + " items";
  const badge = $(".ghost-badge", g);
  if (drag.items.length > 1) { badge.textContent = "+" + drag.items.length; badge.classList.add("show"); }
  else badge.classList.remove("show");
  g.classList.add("visible");
  g.classList.remove("settling");
  document.body.style.userSelect = "none";
  // dim the dragged rows
  for (const it of drag.items) {
    const r = rowEl(drag.srcIdx, it.name);
    if (r) r.classList.add("dragging");
  }
}

function positionGhost(x, y) {
  $("#ghost").style.transform = `translate(${x + 14}px, ${y + 12}px)`;
}

function clearDropHighlights() {
  $$(".pane.drop-target").forEach((el) => el.classList.remove("drop-target"));
  $$(".row.drop-into").forEach((el) => el.classList.remove("drop-into"));
  $$(".crumb.drop-into").forEach((el) => el.classList.remove("drop-into"));
}

function updateDropTarget(x, y) {
  clearDropHighlights();
  drag.target = null;
  const elUnder = document.elementFromPoint(x, y);
  if (!elUnder) return;

  // crumb drop (move into an ancestor folder of the same pane)
  const crumb = elUnder.closest(".crumb");
  if (crumb && crumb.dataset.path !== undefined && !crumb.classList.contains("current")) {
    const paneEl = crumb.closest(".pane");
    const paneIdx = state.els.findIndex((r) => r.root === paneEl);
    // Only the source pane's crumbs are meaningful drop targets; dropping into
    // the directory the items already live in is a no-op.
    if (paneIdx === drag.srcIdx && crumb.dataset.path !== pane(paneIdx).path) {
      crumb.classList.add("drop-into");
      drag.target = { kind: "crumb", paneIdx, path: crumb.dataset.path, el: crumb };
      return;
    }
  }

  const paneEl = elUnder.closest(".pane");
  if (!paneEl) return;
  const paneIdx = state.els.findIndex((r) => r.root === paneEl);
  if (paneIdx < 0) return;

  // folder row drop?
  const row = elUnder.closest(".row");
  if (row && row.classList.contains("dir")) {
    const isDraggingThis = paneIdx === drag.srcIdx && drag.items.some((it) => it.name === row.dataset.name);
    if (!isDraggingThis) {
      row.classList.add("drop-into");
      drag.target = { kind: "folder", paneIdx, name: row.dataset.name, el: row };
      return;
    }
  }

  // whole-pane drop (only meaningful cross-pane; same-pane background = no-op)
  if (paneIdx !== drag.srcIdx) {
    paneEl.classList.add("drop-target");
    drag.target = { kind: "pane", paneIdx, el: paneEl };
  }
}

async function onDragUp(e) {
  window.removeEventListener("pointermove", onDragMove);
  window.removeEventListener("pointerup", onDragUp);
  if (!drag.active) return;

  const wasStarted = drag.started;
  const target = drag.target;
  const items = drag.items;
  const srcIdx = drag.srcIdx;

  // un-dim rows
  for (const it of items) {
    const r = rowEl(srcIdx, it.name);
    if (r) r.classList.remove("dragging");
  }
  document.body.style.userSelect = "";

  drag.active = false;
  drag.started = false;

  if (!wasStarted) { hideGhost(false); clearDropHighlights(); return; }

  if (!target) {
    // invalid drop → snap back
    hideGhost(false);
    clearDropHighlights();
    return;
  }

  // settle ghost into the target, then act
  settleGhostTo(target.el, e.clientX, e.clientY);
  clearDropHighlights();

  if (target.kind === "crumb") {
    // Same-pane move into an ancestor folder (rename within the server).
    if (target.path !== pane(srcIdx).path) {
      await moveSelectionToDir(srcIdx, items, target.path);
    }
    return;
  }

  if (target.kind === "folder") {
    const dstDir = joinPath(pane(target.paneIdx).path, target.name);
    if (target.paneIdx === srcIdx) {
      // same-pane drop on folder → move (rename)
      await moveIntoFolder(srcIdx, items, target.name);
    } else {
      await showDropMenu(srcIdx, items, target.paneIdx, dstDir, e.clientX, e.clientY);
    }
    return;
  }

  if (target.kind === "pane") {
    // cross-pane drop on background → copy/move popup
    await showDropMenu(srcIdx, items, target.paneIdx, pane(target.paneIdx).path, e.clientX, e.clientY);
  }
}

// same-server move into an arbitrary dir (used by crumb drop within a pane)
async function moveSelectionToDir(i, items, dstDir) {
  const p = pane(i);
  let moved = 0;
  for (const item of items) {
    try {
      await postJSON("/api/mv", {
        server: p.server,
        from: joinPath(p.path, item.name),
        to: joinPath(dstDir, item.name),
      });
      moved++;
    } catch (e) {
      toast("Move failed (" + item.name + "): " + e.message, "error");
    }
  }
  if (moved) toast("Moved " + moved + (moved === 1 ? " item" : " items"), "success");
  p.sel.clear();
  loadListing(i);
}

function hideGhost(settling) {
  const g = $("#ghost");
  if (settling) {
    g.classList.add("settling");
    setTimeout(() => { g.classList.remove("visible", "settling"); }, 200);
  } else {
    g.classList.remove("visible", "settling");
  }
}

function settleGhostTo(targetEl, x, y) {
  const g = $("#ghost");
  if (targetEl) {
    const rect = targetEl.getBoundingClientRect();
    const tx = rect.left + rect.width / 2 - 20;
    const ty = rect.top + rect.height / 2 - 14;
    g.style.transform = `translate(${tx}px, ${ty}px) scale(0.6)`;
  }
  hideGhost(true);
}

// drop-action popup (Copy here / Move here / Cancel)
function showDropMenu(srcIdx, items, dstPaneIdx, dstDir, x, y) {
  return new Promise((resolve) => {
    closeMenus();
    const menu = $("#dropmenu");
    const title = document.createElement("div");
    title.className = "dropmenu-title";
    const what = items.length === 1 ? items[0].name : items.length + " items";
    title.textContent = what + "  →  " + dstDir;
    menu.appendChild(title);

    menu.appendChild(makeMenuItem("Copy here", ICONS.copy, async () => {
      await runTransfer(srcIdx, items, dstPaneIdx, dstDir, "copy");
      resolve();
    }));
    menu.appendChild(makeMenuItem("Move here", ICONS.move, async () => {
      await runTransfer(srcIdx, items, dstPaneIdx, dstDir, "move");
      resolve();
    }));
    menu.appendChild(menuSep());
    menu.appendChild(makeMenuItem("Cancel", null, () => resolve()));

    positionFloating(menu, x, y);
  });
}

// ----------------------------------------------------------------- file drop (upload)

function setupFileDropZone(i) {
  const el = state.els[i].root;
  const isFileDrag = (e) => e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files");

  let depth = 0;
  el.addEventListener("dragenter", (e) => {
    if (!isFileDrag(e)) return;
    e.preventDefault();
    depth++;
    setActive(i);
    el.classList.add("file-drag");
  });
  el.addEventListener("dragover", (e) => {
    if (!isFileDrag(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
  });
  el.addEventListener("dragleave", (e) => {
    if (!isFileDrag(e)) return;
    depth--;
    if (depth <= 0) { depth = 0; el.classList.remove("file-drag"); }
  });
  el.addEventListener("drop", (e) => {
    if (!isFileDrag(e)) return;
    e.preventDefault();
    depth = 0;
    el.classList.remove("file-drag");
    const files = collectFiles(e.dataTransfer);
    if (files.length) uploadFiles(i, files);
  });
}

function collectFiles(dt) {
  // Plain files; folder-entry traversal is a nice-to-have we skip for now.
  return dt.files && dt.files.length ? Array.from(dt.files) : [];
}

// ----------------------------------------------------------------- keyboard

function setupKeyboard() {
  document.addEventListener("keydown", (e) => {
    // the editor overlay and modal handle their own keys
    if (editor.open) return;
    if (!$("#modal-overlay").hidden) return;
    if (!$("#ctxmenu").hidden || !$("#dropmenu").hidden) {
      if (e.key === "Escape") closeMenus();
      return;
    }
    const i = state.active;
    const p = pane(i);

    // Global chords that should work even from the filter box.
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "f") {
      e.preventDefault();
      const fi = state.els[i] && state.els[i].filterInput;
      if (fi) fi.focus();
      return;
    }

    const tag = (e.target.tagName || "").toLowerCase();
    if (tag === "input" || tag === "select" || tag === "textarea") {
      // Allow Escape to blur a focused filter box.
      if (e.key === "Escape" && e.target.classList.contains("filter-input")) e.target.blur();
      return;
    }

    if (e.key === "Escape") { p.sel.clear(); renderSelection(i); return; }
    if ((e.key === "v" || e.key === "V") && !e.metaKey && !e.ctrlKey && !e.altKey) {
      e.preventDefault();
      setView(i, p.view === "list" ? "icons" : "list");
      return;
    }
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "t") {
      e.preventDefault();
      addPane();
      return;
    }
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "w" && state.panes.length > 1) {
      e.preventDefault();
      removePane(i);
      return;
    }
    if ((e.key === "Delete" || e.key === "Backspace") && p.sel.size) { e.preventDefault(); actDelete(i); return; }
    if (e.key === "F2" && p.sel.size === 1) { e.preventDefault(); actRename(i); return; }
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "a") {
      e.preventDefault();
      selectAll(i);
      return;
    }
    if (e.key === "Enter" && p.sel.size === 1) {
      const item = p.items.find((x) => p.sel.has(x.name));
      if (item) {
        if (item.is_dir) navigate(i, joinPath(p.path, item.name));
        else if (isTextFile(item.name)) openEditor(i, item);
        else downloadOne(i, item);
      }
      return;
    }
    if (e.key === "Backspace") { navigate(i, parentPath(p.path)); return; }
    if ((e.key === "ArrowDown" || e.key === "ArrowUp") && p.items.length) {
      e.preventDefault();
      moveSelectionByArrow(i, e.key === "ArrowDown" ? 1 : -1, e.shiftKey);
    }
  });
}

function moveSelectionByArrow(i, dir, extend) {
  const p = pane(i);
  const names = visibleItems(p).map((x) => x.name);
  if (!names.length) return;
  let idx = p._anchor != null ? names.indexOf(p._anchor) : -1;
  idx = idx < 0 ? (dir > 0 ? 0 : names.length - 1) : Math.min(names.length - 1, Math.max(0, idx + dir));
  const name = names[idx];
  if (extend) { p.sel.add(name); p._anchor = name; }
  else { p.sel.clear(); p.sel.add(name); p._anchor = name; }
  renderSelection(i);
  const row = rowEl(i, name);
  if (row) row.scrollIntoView({ block: "nearest" });
}

// ----------------------------------------------------------------- copy path

// copyPath puts a file/folder's full path on the clipboard. Falls back to a
// hidden textarea + execCommand where the async Clipboard API is unavailable
// (it needs a secure context; http://localhost qualifies in most browsers).
async function copyPath(i, item) {
  const full = joinPath(pane(i).path, item.name);
  let ok = false;
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(full);
      ok = true;
    }
  } catch { /* fall through to legacy path */ }
  if (!ok) {
    const ta = document.createElement("textarea");
    ta.value = full;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try { ok = document.execCommand("copy"); } catch { ok = false; }
    ta.remove();
  }
  toast(ok ? "Copied path" : "Path: " + full, ok ? "success" : "");
}

// ----------------------------------------------------------------- archive / permissions / checksum / duplicate

// Cache the controller's supported archive formats (fetched lazily on first use).
let archiveFormats = null;
async function loadArchiveFormats() {
  if (archiveFormats) return archiveFormats;
  try {
    archiveFormats = await getJSON("/api/formats");
  } catch {
    archiveFormats = ["zip", "tar.gz", "tar.bz2", "tar.xz", "tar"];
  }
  return archiveFormats;
}

// actCompress opens a dialog (format + archive name) for the current selection,
// then POSTs /api/compress with the selected base names and the pane's dir.
async function actCompress(i) {
  const items = selectedItems(i);
  if (!items.length) { toast("Select items to compress", "error"); return; }
  const formats = await loadArchiveFormats();
  openCompressModal({
    items,
    formats,
    onOk: async (archive, format, names) => {
      const p = pane(i);
      const params = new URLSearchParams();
      params.set("server", p.server);
      params.set("dir", p.path);
      params.set("archive", archive);
      params.set("format", format);
      for (const n of names) params.append("name", n);
      try {
        await postForm("/api/compress", params);
        toast("Created " + archive, "success");
        loadListing(i);
      } catch (e) {
        toast("Compress failed: " + e.message, "error");
      }
    },
  });
}

// actExtract unpacks an archive file into its containing directory.
async function actExtract(i, item) {
  const p = pane(i);
  try {
    await postJSON("/api/extract", { server: p.server, path: joinPath(p.path, item.name) });
    toast("Extracted " + item.name, "success");
    loadListing(i);
  } catch (e) {
    toast("Extract failed: " + e.message, "error");
  }
}

// actChmod prompts for an octal mode and applies it to the selected item.
function actChmod(i, item) {
  const p = pane(i);
  const cur = item.mode != null ? (item.mode & 0o777).toString(8).padStart(3, "0") : "644";
  openInputModal({
    title: "Permissions",
    desc: "Octal mode for “" + item.name + "” (e.g. 644, 755)",
    value: cur,
    okLabel: "Apply",
    onOk: async (mode) => {
      if (!/^[0-7]{3,4}$/.test(mode.trim())) { toast("Mode must be octal (e.g. 644)", "error"); return; }
      try {
        await postJSON("/api/chmod", { server: p.server, path: joinPath(p.path, item.name), mode: mode.trim() });
        toast("Set mode " + mode.trim(), "success");
        loadListing(i);
      } catch (e) {
        toast("Permissions failed: " + e.message, "error");
      }
    },
  });
}

// actChecksum fetches the SHA-256 of a file and shows it in a modal with a copy
// button.
async function actChecksum(i, item) {
  const p = pane(i);
  toast("Computing SHA-256…");
  let out;
  try {
    out = await getJSON("/api/checksum", { server: p.server, path: joinPath(p.path, item.name) });
  } catch (e) {
    toast("Checksum failed: " + e.message, "error");
    return;
  }
  showChecksumModal(item.name, out.sha256 || "");
}

// actDuplicate copies the selected item to a "<name> copy.<ext>" sibling.
async function actDuplicate(i, item) {
  const p = pane(i);
  try {
    await postJSON("/api/duplicate", { server: p.server, path: joinPath(p.path, item.name) });
    toast("Duplicated " + item.name, "success");
    loadListing(i);
  } catch (e) {
    toast("Duplicate failed: " + e.message, "error");
  }
}

// postForm sends an application/x-www-form-urlencoded POST (used by /api/compress
// so repeated `name` params travel in the body). Token rides the header + query.
async function postForm(pathname, params) {
  const res = await fetch(api(pathname, {}), {
    method: "POST",
    headers: { "X-Fleet-Token": TOKEN, "Content-Type": "application/x-www-form-urlencoded" },
    body: params.toString(),
  });
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.json();
}

// openCompressModal — a small dialog with a format <select> and an archive-name
// input. The name's extension auto-tracks the chosen format until the user edits
// the base manually.
function openCompressModal({ items, formats, onOk }) {
  const names = items.map((it) => it.name);
  const base = defaultArchiveBase(items);
  const modal = openModalShell();
  const h = document.createElement("h3"); h.textContent = "Compress"; modal.appendChild(h);
  const d = document.createElement("p"); d.className = "modal-desc";
  d.textContent = items.length === 1 ? "Archive “" + items[0].name + "”" : "Archive " + items.length + " items";
  modal.appendChild(d);

  const fieldFmt = document.createElement("label");
  fieldFmt.className = "modal-field";
  const fmtLabel = document.createElement("span"); fmtLabel.className = "mf-label"; fmtLabel.textContent = "Format";
  const select = document.createElement("select");
  select.className = "modal-select";
  for (const f of formats) {
    const opt = document.createElement("option");
    opt.value = f; opt.textContent = f;
    select.appendChild(opt);
  }
  fieldFmt.append(fmtLabel, select);

  const fieldName = document.createElement("label");
  fieldName.className = "modal-field";
  const nameLabel = document.createElement("span"); nameLabel.className = "mf-label"; nameLabel.textContent = "Archive name";
  const input = document.createElement("input");
  input.type = "text"; input.spellcheck = false;
  fieldName.append(nameLabel, input);

  modal.append(fieldFmt, fieldName);

  let nameEdited = false;
  const apply = () => { input.value = base + "." + select.value; };
  apply();
  select.addEventListener("change", () => { if (!nameEdited) apply(); });
  input.addEventListener("input", () => { nameEdited = true; });

  const actions = document.createElement("div"); actions.className = "modal-actions";
  const cancel = mkBtn("Cancel", "", closeModal);
  const ok = mkBtn("Compress", "primary", submit);
  actions.append(cancel, ok);
  modal.appendChild(actions);

  function submit() {
    const archive = input.value.trim();
    if (!archive) { input.focus(); return; }
    closeModal();
    onOk(archive, select.value, names);
  }
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); submit(); }
    if (e.key === "Escape") { e.preventDefault(); closeModal(); }
  });
  showModal();
  select.focus();
}

// showChecksumModal renders a SHA-256 hash with a one-click copy button.
function showChecksumModal(name, hash) {
  const modal = openModalShell();
  const h = document.createElement("h3"); h.textContent = "SHA-256"; modal.appendChild(h);
  const d = document.createElement("p"); d.className = "modal-desc"; d.textContent = name; modal.appendChild(d);
  const box = document.createElement("div");
  box.className = "checksum-box";
  box.textContent = hash;
  modal.appendChild(box);
  const actions = document.createElement("div"); actions.className = "modal-actions";
  const copyBtn = mkBtn("Copy", "", async () => {
    const okCopy = await copyText(hash);
    copyBtn.textContent = okCopy ? "Copied" : "Copy";
    if (okCopy) setTimeout(() => { copyBtn.textContent = "Copy"; }, 1400);
  });
  const close = mkBtn("Close", "primary", closeModal);
  actions.append(copyBtn, close);
  modal.appendChild(actions);
  showModal();
  close.focus();
}

// copyText copies a string to the clipboard, falling back to a hidden textarea
// where the async Clipboard API is unavailable.
async function copyText(text) {
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch { /* fall through */ }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { ok = false; }
  ta.remove();
  return ok;
}

// ----------------------------------------------------------------- syntax highlighter
//
// A small, self-contained, CSP-clean tokenizer. It classifies common languages
// into spans we colour via app.css — "good enough" highlighting, no eval, no
// external deps. Everything is escaped before being wrapped, so file content is
// never interpreted as HTML.

const EXT_LANG = {
  go: "go",
  js: "js", mjs: "js", cjs: "js", jsx: "js", ts: "js", tsx: "js",
  py: "py", pyw: "py",
  json: "json",
  yaml: "yaml", yml: "yaml",
  toml: "toml", ini: "toml", cfg: "toml", conf: "toml",
  md: "md", markdown: "md",
  sh: "sh", bash: "sh", zsh: "sh", fish: "sh",
  html: "xml", htm: "xml", xml: "xml", svg: "xml", vue: "xml",
  css: "css", scss: "css", less: "css",
  c: "c", h: "c", cpp: "c", cc: "c", hpp: "c", cxx: "c",
  rs: "rust",
  java: "c", kt: "c", swift: "c", php: "c",
  rb: "ruby",
  sql: "sql",
  dockerfile: "sh", makefile: "sh", env: "toml",
  txt: "text", log: "text",
};

// Extensions/known basenames we treat as editable text.
function extOf(name) {
  const base = name.toLowerCase();
  if (base === "dockerfile" || base === "makefile" || base === "rakefile") return base;
  if (base.startsWith(".") && base.indexOf(".", 1) < 0) return base.slice(1); // .gitignore etc
  const dot = base.lastIndexOf(".");
  return dot >= 0 ? base.slice(dot + 1) : "";
}

const TEXT_EXTS = new Set([
  ...Object.keys(EXT_LANG),
  "gitignore", "gitattributes", "editorconfig", "npmrc", "nvmrc", "prettierrc",
  "eslintrc", "babelrc", "properties", "gradle", "tf", "tfvars", "graphql", "gql",
  "csv", "tsv", "rst", "tex", "lua", "pl", "r", "scala", "clj", "ex", "exs", "erl",
  "dart", "groovy", "ps1", "bat", "cmd", "patch", "diff", "lock", "service", "desktop",
]);

function isTextFile(name) {
  const e = extOf(name);
  if (!e) return false;
  return TEXT_EXTS.has(e) || EXT_LANG[e] !== undefined;
}

function langFor(name) {
  return EXT_LANG[extOf(name)] || "text";
}

function langLabel(name) {
  const map = {
    go: "Go", js: "JavaScript", py: "Python", json: "JSON", yaml: "YAML",
    toml: "TOML", md: "Markdown", sh: "Shell", xml: "HTML/XML", css: "CSS",
    c: "C-like", rust: "Rust", ruby: "Ruby", sql: "SQL", text: "Text",
  };
  return map[langFor(name)] || "Text";
}

function escHTML(s) {
  return s.replace(/[&<>]/g, (c) => (c === "&" ? "&amp;" : c === "<" ? "&lt;" : "&gt;"));
}

// Keyword sets per language family.
const KEYWORDS = {
  go: "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false iota append cap close complex copy delete imag len make new panic print println real recover string int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr byte rune float32 float64 bool error any",
  js: "abstract async await break case catch class const continue debugger default delete do else enum export extends false finally for from function get if implements import in instanceof interface let new null of package private protected public return set static super switch this throw true try typeof var void while with yield undefined NaN Infinity console document window",
  py: "and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield True False None self print len range str int float dict list set tuple bool",
  c: "auto break case char const continue default do double else enum extern float for goto if inline int long register return short signed sizeof static struct switch typedef union unsigned void volatile while bool true false class public private protected new delete this namespace using template virtual override final nullptr include define import package",
  rust: "as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while async dyn String Vec Option Some None Result Ok Err Box",
  ruby: "alias and begin break case class def defined do else elsif end ensure false for if in module next nil not or redo rescue retry return self super then true undef unless until when while yield require puts attr_accessor",
  sql: "select from where insert into values update set delete create table drop alter add column primary key foreign references join inner left right outer on group by order having limit offset as and or not null distinct count sum avg min max union index view database",
};

// highlightCode → HTML string with <span class="hl-*"> wrappers. The strategy:
// 1) pull out comments/strings first (so keywords inside them aren't matched),
// 2) then mark numbers and keywords on the remaining plain text.
function highlightCode(src, lang) {
  if (lang === "json") return hlJSON(src);
  if (lang === "yaml" || lang === "toml") return hlConfig(src, lang);
  if (lang === "md") return hlMarkdown(src);
  if (lang === "xml") return hlXML(src);
  if (lang === "css") return hlCSS(src);
  if (lang === "text") return escHTML(src);
  return hlGeneric(src, lang);
}

// Tokenize by scanning for the earliest "interesting" construct (comment or
// string) and emitting plain (keyword/number-marked) text between matches.
function hlGeneric(src, lang) {
  const kw = (KEYWORDS[lang] || KEYWORDS.c).split(" ");
  const kwSet = new Set(kw);
  const lineComment = lang === "py" || lang === "ruby" || lang === "sh" ? "#" : "//";
  let out = "";
  let i = 0;
  const n = src.length;
  while (i < n) {
    const c = src[i];
    const two = src.substr(i, 2);
    // block comment /* ... */
    if (two === "/*") {
      const end = src.indexOf("*/", i + 2);
      const stop = end < 0 ? n : end + 2;
      out += span("cm", src.slice(i, stop));
      i = stop;
      continue;
    }
    // line comment
    if (src.startsWith(lineComment, i) || (lang === "sql" && two === "--")) {
      const marker = lang === "sql" && two === "--" ? "--" : lineComment;
      const end = src.indexOf("\n", i);
      const stop = end < 0 ? n : end;
      out += span("cm", src.slice(i, stop));
      i = stop;
      continue;
    }
    // strings: ' " `
    if (c === '"' || c === "'" || c === "`") {
      const stop = scanString(src, i, c);
      out += span("st", src.slice(i, stop));
      i = stop;
      continue;
    }
    // identifier / keyword
    if (/[A-Za-z_$]/.test(c)) {
      let j = i + 1;
      while (j < n && /[A-Za-z0-9_$]/.test(src[j])) j++;
      const word = src.slice(i, j);
      out += kwSet.has(word) ? span("kw", word) : escHTML(word);
      i = j;
      continue;
    }
    // number
    if (/[0-9]/.test(c) || (c === "." && /[0-9]/.test(src[i + 1] || ""))) {
      let j = i + 1;
      while (j < n && /[0-9a-fA-FxX._]/.test(src[j])) j++;
      out += span("nm", src.slice(i, j));
      i = j;
      continue;
    }
    out += escHTML(c);
    i++;
  }
  return out;
}

// scanString returns the index just past a string literal that starts at `start`
// with quote `q`, honouring backslash escapes (and never crossing a newline for
// ' or ").
function scanString(src, start, q) {
  let j = start + 1;
  const n = src.length;
  while (j < n) {
    const ch = src[j];
    if (ch === "\\") { j += 2; continue; }
    if (ch === q) return j + 1;
    if ((q === '"' || q === "'") && ch === "\n") return j; // unterminated
    j++;
  }
  return n;
}

function span(cls, text) {
  return '<span class="hl-' + cls + '">' + escHTML(text) + "</span>";
}

function hlJSON(src) {
  let out = "";
  let i = 0;
  const n = src.length;
  while (i < n) {
    const c = src[i];
    if (c === '"') {
      const stop = scanString(src, i, '"');
      // a string followed by ':' is a key
      let k = stop;
      while (k < n && /\s/.test(src[k])) k++;
      const cls = src[k] === ":" ? "ky" : "st";
      out += span(cls, src.slice(i, stop));
      i = stop;
      continue;
    }
    if (/[0-9-]/.test(c) && (i === 0 || /[\s,:[]/.test(src[i - 1]))) {
      let j = i + 1;
      while (j < n && /[0-9.eE+-]/.test(src[j])) j++;
      out += span("nm", src.slice(i, j));
      i = j;
      continue;
    }
    if (/[a-z]/.test(c)) {
      let j = i + 1;
      while (j < n && /[a-z]/.test(src[j])) j++;
      const word = src.slice(i, j);
      out += (word === "true" || word === "false" || word === "null") ? span("kw", word) : escHTML(word);
      i = j;
      continue;
    }
    out += escHTML(c);
    i++;
  }
  return out;
}

// hlConfig handles YAML/TOML line-by-line: comments, keys, strings, numbers.
function hlConfig(src, lang) {
  return src.split("\n").map((line) => {
    const hash = line.indexOf("#");
    let code = line, comment = "";
    if (hash >= 0) { code = line.slice(0, hash); comment = line.slice(hash); }
    // key: value  /  key = value
    const m = code.match(/^(\s*[-]?\s*)([A-Za-z0-9_.$-]+|"[^"]*")(\s*[:=]\s*)(.*)$/);
    let html;
    if (m) {
      html = escHTML(m[1]) + span("ky", m[2]) + escHTML(m[3]) + hlConfigVal(m[4]);
    } else {
      html = escHTML(code);
    }
    return html + (comment ? span("cm", comment) : "");
  }).join("\n");
}

function hlConfigVal(v) {
  const t = v.trim();
  if (/^(true|false|null|yes|no|on|off|~)$/i.test(t)) return escHTML(v.slice(0, v.indexOf(t))) + span("kw", t) + escHTML(v.slice(v.indexOf(t) + t.length));
  if (/^-?\d+(\.\d+)?$/.test(t)) return escHTML(v.replace(t, "")) + span("nm", t);
  if (/^["'].*["']$/.test(t)) return span("st", v);
  return escHTML(v);
}

function hlMarkdown(src) {
  return src.split("\n").map((line) => {
    if (/^\s*#{1,6}\s/.test(line)) return span("kw", line);
    if (/^\s*([-*+]|\d+\.)\s/.test(line)) {
      const m = line.match(/^(\s*)([-*+]|\d+\.)(\s.*)$/);
      if (m) return escHTML(m[1]) + span("ky", m[2]) + hlInlineMd(m[3]);
    }
    if (/^\s*>/.test(line)) return span("cm", line);
    if (/^\s*```/.test(line)) return span("st", line);
    return hlInlineMd(line);
  }).join("\n");
}

function hlInlineMd(s) {
  // escape, then re-introduce highlight spans for `code`, **bold**, [links]
  let out = escHTML(s);
  out = out.replace(/`[^`]+`/g, (m) => span("st", m.replace(/^`|`$/g, "`")));
  out = out.replace(/\*\*[^*]+\*\*/g, (m) => '<span class="hl-kw">' + m + "</span>");
  out = out.replace(/\[[^\]]+\]\([^)]+\)/g, (m) => '<span class="hl-ky">' + m + "</span>");
  return out;
}

function hlXML(src) {
  let out = "";
  let i = 0;
  const n = src.length;
  while (i < n) {
    if (src.startsWith("<!--", i)) {
      const end = src.indexOf("-->", i);
      const stop = end < 0 ? n : end + 3;
      out += span("cm", src.slice(i, stop));
      i = stop;
      continue;
    }
    if (src[i] === "<") {
      const end = src.indexOf(">", i);
      const stop = end < 0 ? n : end + 1;
      out += hlTag(src.slice(i, stop));
      i = stop;
      continue;
    }
    const lt = src.indexOf("<", i);
    const stop = lt < 0 ? n : lt;
    out += escHTML(src.slice(i, stop));
    i = stop;
  }
  return out;
}

function hlTag(tag) {
  // <tag attr="val"> → tag name + attrs + strings
  let out = "&lt;";
  let body = tag.slice(1, tag.endsWith(">") ? -1 : undefined);
  let closing = "";
  if (tag.endsWith(">")) closing = "&gt;";
  const nameMatch = body.match(/^\/?[A-Za-z0-9:-]+/);
  if (nameMatch) {
    out += span("ky", nameMatch[0]);
    body = body.slice(nameMatch[0].length);
  }
  out += body.replace(/("[^"]*"|'[^']*')/g, (m) => span("st", m))
             .replace(/([A-Za-z-]+)(=)/g, (m, a, eq) => '<span class="hl-nm">' + escHTML(a) + "</span>" + escHTML(eq));
  return out + closing;
}

function hlCSS(src) {
  let out = "";
  let i = 0;
  const n = src.length;
  while (i < n) {
    if (src.startsWith("/*", i)) {
      const end = src.indexOf("*/", i + 2);
      const stop = end < 0 ? n : end + 2;
      out += span("cm", src.slice(i, stop));
      i = stop;
      continue;
    }
    const c = src[i];
    if (c === '"' || c === "'") {
      const stop = scanString(src, i, c);
      out += span("st", src.slice(i, stop));
      i = stop;
      continue;
    }
    if (c === "#" || c === ".") {
      let j = i + 1;
      while (j < n && /[A-Za-z0-9_-]/.test(src[j])) j++;
      out += span("ky", src.slice(i, j));
      i = j;
      continue;
    }
    if (/[0-9]/.test(c)) {
      let j = i + 1;
      while (j < n && /[0-9.a-z%]/.test(src[j])) j++;
      out += span("nm", src.slice(i, j));
      i = j;
      continue;
    }
    out += escHTML(c);
    i++;
  }
  return out;
}

// ----------------------------------------------------------------- editor

const editor = {
  open: false,
  paneIdx: -1,
  item: null,
  original: "",
  lang: "text",
};

async function openEditor(i, item) {
  const p = pane(i);
  const full = joinPath(p.path, item.name);
  const overlay = $("#editor-overlay");
  const ta = $("#ed-input");
  const name = $("#ed-name");
  const lang = $("#ed-lang");
  const meta = $("#ed-meta");
  const iconWrap = $(".ed-icon", overlay);

  iconWrap.innerHTML = ICONS.file;
  name.textContent = item.name;
  lang.textContent = langLabel(item.name);
  meta.textContent = "Loading…";
  ta.value = "";
  ta.disabled = true;
  setDirty(false);
  highlightEditor("", langFor(item.name));
  overlay.hidden = false;
  editor.open = true;
  editor.paneIdx = i;
  editor.item = item;
  editor.lang = langFor(item.name);

  let data;
  try {
    data = await getJSON("/api/read", { server: p.server, path: full });
  } catch (e) {
    meta.textContent = "";
    toast("Open failed: " + e.message, "error");
    closeEditor();
    return;
  }
  if (!editor.open || editor.item !== item) return; // closed while loading
  editor.original = data.content || "";
  ta.value = editor.original;
  ta.disabled = false;
  meta.textContent = humanSize(data.size || editor.original.length);
  syncEditorView();
  setDirty(false);
  ta.focus();
}

function highlightEditor(text, lang) {
  const code = $("#ed-highlight code");
  // A trailing newline keeps the highlight layer's height aligned with the
  // textarea when the file ends with \n.
  code.innerHTML = highlightCode(text, lang) + "\n";
}

// syncEditorView re-highlights and rebuilds the line-number gutter from the
// textarea's current content, and mirrors scroll between the two layers.
function syncEditorView() {
  const ta = $("#ed-input");
  highlightEditor(ta.value, editor.lang);
  const lines = ta.value.split("\n").length;
  const gutter = $("#ed-gutter");
  let g = "";
  for (let k = 1; k <= lines; k++) g += k + "\n";
  gutter.textContent = g;
  syncEditorScroll();
}

function syncEditorScroll() {
  const ta = $("#ed-input");
  const hl = $("#ed-highlight");
  const gutter = $("#ed-gutter");
  hl.scrollTop = ta.scrollTop;
  hl.scrollLeft = ta.scrollLeft;
  gutter.scrollTop = ta.scrollTop;
}

function setDirty(d) {
  editor.dirty = d;
  $("#ed-dirty").hidden = !d;
}

function closeEditor() {
  $("#editor-overlay").hidden = true;
  editor.open = false;
  editor.item = null;
  editor.paneIdx = -1;
  $("#ed-input").value = "";
}

async function saveEditor() {
  if (!editor.open) return;
  const i = editor.paneIdx;
  const p = pane(i);
  const item = editor.item;
  const ta = $("#ed-input");
  const content = ta.value;
  const full = joinPath(p.path, item.name);
  const saveBtn = $("#ed-save");
  saveBtn.disabled = true;
  saveBtn.textContent = "Saving…";
  try {
    const res = await fetch(api("/api/write", { server: p.server, path: full }), {
      method: "POST",
      headers: { "X-Fleet-Token": TOKEN, "Content-Type": "text/plain" },
      body: content,
    });
    if (!res.ok) throw new Error((await res.text()) || res.statusText);
  } catch (e) {
    toast("Save failed: " + e.message, "error");
    saveBtn.disabled = false;
    saveBtn.textContent = "Save";
    return;
  }
  editor.original = content;
  setDirty(false);
  saveBtn.disabled = false;
  saveBtn.textContent = "Save";
  toast("Saved " + item.name, "success");
  loadListing(i);
}

function setupEditor() {
  const ta = $("#ed-input");
  ta.addEventListener("input", () => {
    syncEditorView();
    setDirty(ta.value !== editor.original);
  });
  ta.addEventListener("scroll", syncEditorScroll);
  // Tab inserts a soft tab instead of leaving the textarea.
  ta.addEventListener("keydown", (e) => {
    if (e.key === "Tab") {
      e.preventDefault();
      const s = ta.selectionStart, en = ta.selectionEnd;
      ta.value = ta.value.slice(0, s) + "  " + ta.value.slice(en);
      ta.selectionStart = ta.selectionEnd = s + 2;
      syncEditorView();
      setDirty(ta.value !== editor.original);
    }
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
      e.preventDefault();
      saveEditor();
    }
    if (e.key === "Escape") { e.preventDefault(); tryCloseEditor(); }
  });
  $("#ed-save").addEventListener("click", saveEditor);
  $("#ed-cancel").addEventListener("click", tryCloseEditor);
}

function tryCloseEditor() {
  if (editor.dirty) {
    confirmAsync({
      title: "Discard changes?",
      desc: "“" + (editor.item ? editor.item.name : "") + "” has unsaved changes.",
      okLabel: "Discard",
    }).then((ok) => { if (ok) closeEditor(); });
    return;
  }
  closeEditor();
}

// ----------------------------------------------------------------- takeover

function showTakeover(title, htmlDesc) {
  const div = document.createElement("div");
  div.className = "takeover";
  const inner = document.createElement("div");
  inner.className = "to-inner";
  const h = document.createElement("h2");
  h.textContent = title;
  const p = document.createElement("p");
  p.innerHTML = htmlDesc; // trusted, static strings only
  inner.append(h, p);
  div.appendChild(inner);
  document.body.appendChild(div);
}

// ----------------------------------------------------------------- init

function init() {
  if (!TOKEN) {
    showTakeover(
      "Missing access token",
      'Open the URL printed by <code>fleet ui</code> — it carries the one-time access token.'
    );
    return;
  }
  buildPanes();
  setupDock();
  setupKeyboard();
  setupEditor();

  $("#add-pane").addEventListener("click", addPane);

  // global dismissers
  document.addEventListener("pointerdown", (e) => {
    if (!$("#ctxmenu").hidden && !e.target.closest("#ctxmenu")) closeMenus();
    if (!$("#dropmenu").hidden && !e.target.closest("#dropmenu")) closeMenus();
  }, true);
  $("#modal-overlay").addEventListener("pointerdown", (e) => {
    if (e.target === $("#modal-overlay")) closeModal();
  });
  window.addEventListener("blur", () => { if (drag.active) onDragUp({ clientX: drag.startX, clientY: drag.startY }); });
  window.addEventListener("resize", closeMenus);

  loadServers();
}

init();
