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
  empty: '<svg viewBox="0 0 24 24"><path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2V7z"/></svg>',
};

// ----------------------------------------------------------------- state

const state = {
  servers: [],
  panes: [
    { server: "", path: "/", items: [], sel: new Set(), hidden: false, loading: false },
    { server: "", path: "/", items: [], sel: new Set(), hidden: false, loading: false },
  ],
  active: 0,
  els: [null, null], // per-pane DOM refs
};

function pane(i) { return state.panes[i]; }
function other(i) { return i === 0 ? 1 : 0; }

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

function buildPanes() {
  const host = $("#panes");
  host.innerHTML = "";
  const tpl = $("#pane-template");
  for (let i = 0; i < 2; i++) {
    const node = tpl.content.firstElementChild.cloneNode(true);
    host.appendChild(node);
    const refs = {
      root: node,
      server: $(".pane-server", node),
      back: $(".tool.back", node),
      refresh: $(".tool.refresh", node),
      mkdir: $(".tool.mkdir", node),
      upload: $(".tool.upload", node),
      hiddenToggle: $(".tool.hidden-toggle", node),
      fileInput: $(".file-input", node),
      crumbs: $(".crumbs", node),
      entries: $(".entries", node),
      listing: $(".listing", node),
      body: $(".pane-body", node),
      selInfo: $(".sel-info", node),
      actDownload: $(".act-download", node),
      actCopy: $(".act-copy", node),
      actMove: $(".act-move", node),
      actRename: $(".act-rename", node),
      actDelete: $(".act-delete", node),
    };
    state.els[i] = refs;
    wirePane(i);
  }
  setActive(0);
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
  r.upload.addEventListener("click", () => { setActive(i); r.fileInput.click(); });
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

  r.actDownload.addEventListener("click", () => actDownload(i));
  r.actCopy.addEventListener("click", () => transferSelection(i, "copy"));
  r.actMove.addEventListener("click", () => transferSelection(i, "move"));
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
  if (state.active === i && state.els[i] && state.els[i].root.classList.contains("active")) return;
  state.active = i;
  for (let k = 0; k < 2; k++) {
    state.els[k].root.classList.toggle("active", k === i);
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
  if (!servers.length) {
    showTakeover(
      "No servers configured",
      'Add a server with <code>fleet server add</code>, then reopen the file manager.'
    );
    return;
  }
  for (let i = 0; i < 2; i++) {
    const sel = state.els[i].server;
    sel.innerHTML = "";
    for (const s of servers) {
      const opt = document.createElement("option");
      opt.value = s.name;
      opt.textContent = s.reachable ? s.name : s.name + " (offline)";
      opt.disabled = !s.reachable;
      sel.appendChild(opt);
    }
    // pane 0 → first reachable; pane 1 → second reachable (or first)
    const reachable = servers.filter((s) => s.reachable);
    let pick = reachable[0] || servers[0];
    if (i === 1 && reachable.length > 1) pick = reachable[1];
    pane(i).server = pick.name;
    sel.value = pick.name;
  }
  await Promise.all([loadListing(0), loadListing(1)]);
}

// ----------------------------------------------------------------- listing

async function loadListing(i) {
  const p = pane(i);
  const r = state.els[i];
  if (!p.server) return;
  p.loading = true;
  showPlaceholder(i, "spinner", "Loading…");
  let result;
  try {
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
  // sort: dirs first, then name (case-insensitive)
  p.items = (result.entries || []).slice().sort((a, b) => {
    if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
    return a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
  });
  renderCrumbs(i);
  renderEntries(i);
  renderSelection(i);
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
  const frag = document.createDocumentFragment();
  for (const item of p.items) {
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
    wireRow(i, row, item);
    frag.appendChild(row);
  }
  r.entries.appendChild(frag);
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
  const sz = p.items.filter((it) => p.sel.has(it.name)).reduce((a, it) => a + (it.is_dir ? 0 : it.size || 0), 0);
  r.selInfo.textContent = n === 0 ? "" : n === 1 ? "1 selected" : `${n} selected · ${humanSize(sz)}`;

  const onlyFiles = n > 0 && p.items.filter((it) => p.sel.has(it.name)).every((it) => !it.is_dir);
  r.actDownload.disabled = !onlyFiles;
  r.actCopy.disabled = n === 0;
  r.actMove.disabled = n === 0;
  r.actRename.disabled = n !== 1;
  r.actDelete.disabled = n === 0;
}

function selectedItems(i) {
  const p = pane(i);
  return p.items.filter((it) => p.sel.has(it.name));
}

function toggleSelect(i, name, opts = {}) {
  const p = pane(i);
  if (opts.range && p.sel.size && p._anchor != null) {
    const names = p.items.map((x) => x.name);
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
  if (!p.server) return;
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
    watchTransfer(resp.id, "Upload " + file.name, "upload", () => loadListing(i));
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
      add(makeMenuItem("Download", ICONS.download, () => actDownload(i)));
    }
    add(makeMenuItem(multi ? "Copy → other pane" : "Copy → other pane", ICONS.copy, () => transferSelection(i, "copy")));
    add(makeMenuItem("Move → other pane", ICONS.move, () => transferSelection(i, "move")));
    add(makeMenuItem("Rename", ICONS.rename, () => actRename(i), { disabled: multi }));
    add(menuSep());
    add(makeMenuItem("Delete", ICONS.trash, () => actDelete(i), { danger: true, key: "⌫" }));
    add(menuSep());
  }
  add(makeMenuItem("New folder", ICONS.newfolder, () => promptMkdir(i)));
  add(makeMenuItem("Upload files…", ICONS.download, () => state.els[i].fileInput.click()));
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
    ["Server", p.server],
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
    // modal handles its own keys
    if (!$("#modal-overlay").hidden) return;
    if (!$("#ctxmenu").hidden || !$("#dropmenu").hidden) {
      if (e.key === "Escape") closeMenus();
      return;
    }
    const i = state.active;
    const p = pane(i);
    const tag = (e.target.tagName || "").toLowerCase();
    if (tag === "input" || tag === "select" || tag === "textarea") return;

    if (e.key === "Escape") { p.sel.clear(); renderSelection(i); return; }
    if ((e.key === "Delete" || e.key === "Backspace") && p.sel.size) { e.preventDefault(); actDelete(i); return; }
    if (e.key === "F2" && p.sel.size === 1) { e.preventDefault(); actRename(i); return; }
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "a") {
      e.preventDefault();
      p.sel = new Set(p.items.map((x) => x.name));
      renderSelection(i);
      return;
    }
    if (e.key === "Enter" && p.sel.size === 1) {
      const item = p.items.find((x) => p.sel.has(x.name));
      if (item) { if (item.is_dir) navigate(i, joinPath(p.path, item.name)); else downloadOne(i, item); }
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
  const names = p.items.map((x) => x.name);
  let idx = p._anchor != null ? names.indexOf(p._anchor) : -1;
  idx = idx < 0 ? (dir > 0 ? 0 : names.length - 1) : Math.min(names.length - 1, Math.max(0, idx + dir));
  const name = names[idx];
  if (extend) { p.sel.add(name); p._anchor = name; }
  else { p.sel.clear(); p.sel.add(name); p._anchor = name; }
  renderSelection(i);
  const row = rowEl(i, name);
  if (row) row.scrollIntoView({ block: "nearest" });
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
