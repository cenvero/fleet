// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh
//
// Cenvero Fleet web file manager + read-only Fleet overview. Vanilla JS, no
// build step, no dependencies, no external requests. The CSP forbids inline
// scripts, inline styles and event-handler attributes, so every behaviour
// lives here, DOM is built with createElement/textContent (never innerHTML
// with untrusted data), and dynamic geometry is set through the CSSOM.

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
  window.history.replaceState(null, "", location.pathname + (location.hash || ""));
}

function api(pathname, params = {}) {
  const url = new URL(pathname, location.origin);
  url.searchParams.set("t", TOKEN);
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== "") url.searchParams.set(k, v);
  }
  return url;
}

// ApiError carries the HTTP status and the server's message (the backend
// answers {"error": "..."} JSON or a plain-text http.Error body).
class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

async function errorFrom(res) {
  let text = "";
  try { text = await res.text(); } catch { /* ignore */ }
  let msg = text.trim();
  try {
    const parsed = JSON.parse(text);
    if (parsed && typeof parsed.error === "string") msg = parsed.error;
  } catch { /* plain text */ }
  return new ApiError(msg || res.statusText || "HTTP " + res.status, res.status);
}

async function getJSON(pathname, params) {
  const res = await fetch(api(pathname, params), { headers: { "X-Fleet-Token": TOKEN } });
  if (!res.ok) throw await errorFrom(res);
  return res.json();
}

async function postJSON(pathname, params) {
  const res = await fetch(api(pathname, params), { method: "POST", headers: { "X-Fleet-Token": TOKEN } });
  if (!res.ok) throw await errorFrom(res);
  return res.json();
}

// postForm sends an application/x-www-form-urlencoded POST (used by /api/compress
// so repeated `name` params travel in the body). Token rides the header + query.
async function postForm(pathname, params) {
  const res = await fetch(api(pathname, {}), {
    method: "POST",
    headers: { "X-Fleet-Token": TOKEN, "Content-Type": "application/x-www-form-urlencoded" },
    body: params.toString(),
  });
  if (!res.ok) throw await errorFrom(res);
  return res.json();
}

// friendlyError turns backend/agent errors into sentences an operator can act on.
function friendlyError(err, server) {
  const msg = String((err && err.message) || err || "Unknown error");
  const where = server ? server : "this machine";
  if (/outside the agent's allowed file roots/i.test(msg)) return "That location is outside " + where + "'s allowed file roots.";
  if (/contains the controller's configuration directory/i.test(msg)) return "That folder contains the controller's configuration directory or SSH keys, so the web UI won't copy, move, rename, archive or delete it as a whole. Select the items you need inside it instead.";
  if (/configuration directory/i.test(msg)) return "The controller's configuration directory and key files (keys, tokens, secrets) aren't available in the web UI.";
  if (/access to .* is not permitted/i.test(msg)) return "The agent on " + where + " doesn't allow access to that location.";
  if (/permission denied/i.test(msg)) return "Permission denied.";
  if (/no such file|not found|does not exist|cannot find/i.test(msg)) return "It no longer exists — it may have been moved or deleted.";
  if (/not a directory/i.test(msg)) return "That is not a folder.";
  if (/connection refused|unreachable|no route|timed? ?out|i\/o timeout|EOF/i.test(msg)) return (server || "The server") + " is not reachable right now.";
  if (/failed to fetch|networkerror/i.test(msg)) return "Lost the connection to the Fleet web UI. Is `fleet file ui` still running?";
  return msg;
}

// ----------------------------------------------------------------- utilities

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
const SVGNS = "http://www.w3.org/2000/svg";
const IS_MAC = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || "");
const MOD = IS_MAC ? "⌘" : "Ctrl";
const modKey = (e) => (IS_MAC ? e.metaKey : e.ctrlKey);
const COLLATOR = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

// h builds an element: h("button", {class: "btn", onclick: fn, "aria-label": "x"}, child…)
function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") el.className = v;
      else if (k === "text") el.textContent = v;
      else if (k === "dataset") Object.assign(el.dataset, v);
      else if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
      else if (k === "value") el.value = v;
      else if (typeof v === "boolean") el[k] = v;
      else el.setAttribute(k, String(v));
    }
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === undefined || kid === null || kid === false) continue;
    el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

function icon(name, cls) {
  const svg = document.createElementNS(SVGNS, "svg");
  svg.setAttribute("class", "i" + (cls ? " " + cls : ""));
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  const use = document.createElementNS(SVGNS, "use");
  use.setAttribute("href", "#i-" + name);
  svg.appendChild(use);
  return svg;
}

function setIcon(svg, name) {
  const use = svg && svg.querySelector("use");
  if (use) use.setAttribute("href", "#i-" + name);
}

function humanSize(n) {
  if (n === undefined || n === null || isNaN(n)) return "";
  if (n < 1024) return n + " B";
  const units = ["KB", "MB", "GB", "TB", "PB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 100 ? 0 : 1) + " " + units[i];
}

function validDate(iso) {
  if (!iso) return null;
  const d = new Date(iso);
  if (isNaN(d.getTime()) || d.getFullYear() < 1971) return null;
  return d;
}

const MONTH_FMT = new Intl.DateTimeFormat(undefined, { month: "short" });

function fmtTime(iso) {
  const d = validDate(iso);
  if (!d) return "";
  const now = new Date();
  const mon = MONTH_FMT.format(d);
  if (d.getFullYear() === now.getFullYear()) {
    const hh = String(d.getHours()).padStart(2, "0");
    const mm = String(d.getMinutes()).padStart(2, "0");
    return `${mon} ${d.getDate()}, ${hh}:${mm}`;
  }
  return `${mon} ${d.getDate()}, ${d.getFullYear()}`;
}

function fmtDateFull(iso) {
  const d = validDate(iso);
  return d ? d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "medium" }) : "—";
}

function fmtRelative(iso) {
  const d = validDate(iso);
  if (!d) return "never";
  const s = Math.round((Date.now() - d.getTime()) / 1000);
  if (s < 5) return "just now";
  if (s < 60) return s + "s ago";
  if (s < 3600) return Math.round(s / 60) + "m ago";
  if (s < 86400) return Math.round(s / 3600) + "h ago";
  return Math.round(s / 86400) + "d ago";
}

function fmtDuration(sec) {
  if (!isFinite(sec) || sec < 0) return "";
  sec = Math.round(sec);
  if (sec < 60) return sec + "s";
  if (sec < 3600) return Math.floor(sec / 60) + "m " + String(sec % 60).padStart(2, "0") + "s";
  return Math.floor(sec / 3600) + "h " + String(Math.floor((sec % 3600) / 60)).padStart(2, "0") + "m";
}

function plural(n, one, many) { return n + " " + (n === 1 ? one : many || one + "s"); }

function debounce(fn, ms) {
  let t = 0;
  const wrapped = (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
  wrapped.cancel = () => clearTimeout(t);
  return wrapped;
}

function randomHex(bytes) {
  const buf = new Uint8Array(bytes);
  crypto.getRandomValues(buf);
  return Array.from(buf, (b) => b.toString(16).padStart(2, "0")).join("");
}

// store wraps localStorage: failures (private mode, quota) never break the UI.
const store = {
  get(key, fallback) {
    try {
      const raw = localStorage.getItem("fleet." + key);
      return raw === null ? fallback : JSON.parse(raw);
    } catch { return fallback; }
  },
  set(key, value) {
    try { localStorage.setItem("fleet." + key, JSON.stringify(value)); } catch { /* ignore */ }
  },
  del(key) {
    try { localStorage.removeItem("fleet." + key); } catch { /* ignore */ }
  },
};

// announce speaks a short status message to screen readers.
function announce(msg) {
  const el = $("#sr-status");
  if (!el) return;
  el.textContent = "";
  setTimeout(() => { el.textContent = msg; }, 30);
}

function reducedMotion() {
  return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

// ----------------------------------------------------------------- path helpers
//
// Target-aware lexical path operations. The backend stays authoritative; these
// mirror its rules so the UI can build paths and validate names before sending.

function normalizePathStyle(style) {
  return style === "windows" ? "windows" : "posix";
}

function splitWindowsPath(input) {
  const value = String(input || "").replace(/\//g, "\\");
  const unc = value.match(/^\\\\([^\\]+)\\([^\\]+)(.*)$/);
  if (unc) return { volume: "\\\\" + unc[1] + "\\" + unc[2], rest: unc[3] || "", rooted: true };
  const drive = value.match(/^([A-Za-z]:)(.*)$/);
  if (drive) return { volume: drive[1], rest: drive[2] || "", rooted: drive[2].startsWith("\\") };
  return { volume: "", rest: value, rooted: value.startsWith("\\") };
}

function cleanPath(input, style) {
  style = normalizePathStyle(style);
  if (style === "posix") {
    const value = String(input || "");
    const rooted = value.startsWith("/");
    const parts = [];
    for (const part of value.split("/")) {
      if (!part || part === ".") continue;
      if (part === "..") {
        if (parts.length && parts[parts.length - 1] !== "..") parts.pop();
        else if (!rooted) parts.push(part);
      } else parts.push(part);
    }
    if (rooted) return "/" + parts.join("/");
    return parts.join("/") || ".";
  }

  const parsed = splitWindowsPath(input);
  const parts = [];
  for (const part of parsed.rest.split(/\\+/)) {
    if (!part || part === ".") continue;
    if (part === "..") {
      if (parts.length && parts[parts.length - 1] !== "..") parts.pop();
      else if (!parsed.rooted) parts.push(part);
    } else parts.push(part);
  }
  const body = parts.join("\\");
  if (parsed.rooted) return parsed.volume + "\\" + body;
  return parsed.volume + body || ".";
}

function joinPath(dir, name, style) {
  style = normalizePathStyle(style);
  if (style === "windows") {
    const next = splitWindowsPath(name);
    if ((next.volume && next.rooted) || next.volume) return cleanPath(name, style);
    return cleanPath(String(dir || "").replace(/[\\/]+$/, "") + "\\" + String(name || "").replace(/^[\\/]+/, ""), style);
  }
  if (String(name || "").startsWith("/")) return cleanPath(name, style);
  return cleanPath(String(dir || "").replace(/\/+$/, "") + "/" + String(name || "").replace(/^\/+/, ""), style);
}

function pathWithin(root, target, style) {
  style = normalizePathStyle(style);
  root = cleanPath(root, style);
  target = cleanPath(target, style);
  if (style === "windows") {
    const r = splitWindowsPath(root);
    const t = splitWindowsPath(target);
    if (!r.rooted || !t.rooted || r.volume.toLowerCase() !== t.volume.toLowerCase()) return false;
    const rp = r.rest.split(/\\+/).filter(Boolean);
    const tp = t.rest.split(/\\+/).filter(Boolean);
    return rp.length <= tp.length && rp.every((part, i) => part.toLowerCase() === tp[i].toLowerCase());
  }
  return root === "/" || target === root || target.startsWith(root.replace(/\/+$/, "") + "/");
}

function rootPath(p, style, fallback = "") {
  style = normalizePathStyle(style);
  if (fallback) return cleanPath(fallback, style);
  if (style === "posix") return "/";
  let parsed = splitWindowsPath(cleanPath(p, style));
  if (!parsed.volume || !parsed.rooted) parsed = splitWindowsPath("C:\\");
  return parsed.volume && parsed.rooted ? parsed.volume + "\\" : "C:\\";
}

function parentPath(p, style, fallback = "") {
  style = normalizePathStyle(style);
  const clean = cleanPath(p, style);
  const root = rootPath(clean, style, fallback);
  if (!pathWithin(root, clean, style) || clean.toLowerCase() === root.toLowerCase()) return root;
  let parent;
  if (style === "posix") {
    const idx = clean.lastIndexOf("/");
    parent = idx <= 0 ? "/" : clean.slice(0, idx);
  } else {
    const parsed = splitWindowsPath(clean);
    const parts = parsed.rest.split(/\\+/).filter(Boolean);
    parts.pop();
    parent = parts.length ? parsed.volume + "\\" + parts.join("\\") : rootPath(clean, style);
  }
  return pathWithin(root, parent, style) ? parent : root;
}

function baseName(p, style) {
  style = normalizePathStyle(style);
  const clean = cleanPath(p, style);
  if (clean.toLowerCase() === rootPath(clean, style).toLowerCase()) return style === "windows" ? "\\" : "/";
  const sep = style === "windows" ? "\\" : "/";
  const idx = clean.lastIndexOf(sep);
  return idx < 0 ? clean : clean.slice(idx + 1);
}

function pathBreadcrumbs(p, style, fallback = "") {
  style = normalizePathStyle(style);
  const clean = cleanPath(p, style);
  const root = rootPath(clean, style, fallback);
  if (!pathWithin(root, clean, style)) return [{ label: root, path: root }];
  let rest;
  if (style === "windows") {
    const rootParts = splitWindowsPath(root).rest.split(/\\+/).filter(Boolean);
    const cleanParts = splitWindowsPath(clean).rest.split(/\\+/).filter(Boolean);
    rest = cleanParts.slice(rootParts.length);
  } else {
    const suffix = root === "/" ? clean : clean.slice(root.length);
    rest = suffix.split("/").filter(Boolean);
  }
  const crumbs = [{ label: root, path: root }];
  let current = root;
  for (const segment of rest) {
    current = joinPath(current, segment, style);
    crumbs.push({ label: segment, path: current });
  }
  return crumbs;
}

// componentNameError mirrors the backend's authoritative one-component rule.
// Both separators are forbidden for every source style; Windows sources also
// enforce Win32-invalid characters, trailing dot/space, and DOS device names.
function componentNameError(name, style) {
  name = String(name == null ? "" : name);
  style = normalizePathStyle(style);
  if (!name) return "Name is required";
  if (name === "." || name === "..") return "Name must not be “" + name + "”";
  if (/^[A-Za-z]:[\\/]/.test(name) || /^[\\/]{2}/.test(name)) return "Name must not be an absolute path";
  if (/[\\/]/.test(name)) return "Name must be one path component";
  if(/[\u0000-\u001f\u007f-\u009f]/u.test(name)) return "Name must not contain control characters";
  if (style !== "windows") return "";
  if (/[<>:\"|?*]/.test(name)) return "Name contains a character invalid on Windows";
  if (/[. ]$/.test(name)) return "Windows names must not end in a dot or space";
  const stem = name.split(".", 1)[0].replace(/[. ]+$/g, "");
  if (/^(con|prn|aux|nul|conin\$|conout\$|(?:com|lpt)(?:[1-9¹²³]))$/i.test(stem)) {
    return "This is a reserved Windows name";
  }
  return "";
}

function batchNamespaceError(items, style, caseInsensitive = normalizePathStyle(style) === "windows") {
  style = normalizePathStyle(style);
  const seen = new Map();
  for (const item of items) {
    const err = componentNameError(item.name, style);
    if (err) return "“" + item.name + "”: " + err;
    const key = caseInsensitive ? item.name.toLowerCase() : item.name;
    if (seen.has(key) && seen.get(key) !== item.name) {
      return "Destination name collision: “" + seen.get(key) + "” and “" + item.name + "”";
    }
    seen.set(key, item.name);
  }
  return "";
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


// ----------------------------------------------------------------- file kinds

const IMAGE_PREVIEW_EXTS = new Set(["png", "jpg", "jpeg", "gif", "webp", "bmp", "ico", "avif"]);
const IMAGE_EXTS = new Set([...IMAGE_PREVIEW_EXTS, "svg", "tif", "tiff", "heic", "psd"]);
const ARCHIVE_EXTS = new Set(["zip", "tar", "gz", "tgz", "bz2", "xz", "7z", "rar", "zst", "tbz2", "txz"]);
const MEDIA_EXTS = new Set(["mp4", "mov", "mkv", "webm", "avi", "mp3", "wav", "flac", "ogg", "m4a", "aac"]);

function kindOf(item) {
  if (!item) return "file";
  if (item.is_dir) return "dir";
  if (item.is_symlink) return "link";
  const e = extOf(item.name);
  if (IMAGE_EXTS.has(e)) return "image";
  if (ARCHIVE_EXTS.has(e) || isArchiveFile(item.name)) return "archive";
  if (MEDIA_EXTS.has(e)) return "media";
  const lang = EXT_LANG[e];
  if (lang && lang !== "text") return "code";
  if (isTextFile(item.name)) return "text";
  return "file";
}

const KIND_ICON = { dir: "folder", link: "link", image: "file-image", archive: "file-archive", media: "file-media", code: "file-code", text: "file-text", file: "file" };

function iconNameFor(item) { return KIND_ICON[kindOf(item)] || "file"; }

function kindLabel(item) {
  const k = kindOf(item);
  if (k === "dir") return "Folder";
  if (k === "link") return "Symbolic link";
  const e = extOf(item.name);
  if (k === "code" || k === "text") return langLabel(item.name) === "Text" ? "Text document" : langLabel(item.name) + " file";
  if (k === "image") return e.toUpperCase() + " image";
  if (k === "archive") return e.toUpperCase() + " archive";
  if (k === "media") return e.toUpperCase() + " media";
  return e ? e.toUpperCase() + " file" : "File";
}

function modeString(mode, isDir) {
  if (mode === undefined || mode === null) return "—";
  const m = mode & 0o777;
  const bits = "rwxrwxrwx";
  let s = isDir ? "d" : "-";
  for (let i = 0; i < 9; i++) s += m & (1 << (8 - i)) ? bits[i] : "-";
  return s + "  (" + m.toString(8).padStart(3, "0") + ")";
}

// ----------------------------------------------------------------- state

let paneSeq = 0;

// Panes are dynamic (1..MAX_PANES). Each carries its own source, path,
// history, selection, view, hidden-toggle, filter text and sort spec. DOM refs
// live on the pane object (p.el) so async work never depends on indices.
function newPaneState(server) {
  return {
    uid: ++paneSeq,
    server: server || "",
    os: "",
    pathStyle: "posix",
    caseInsensitive: false,
    initialRoot: "/",
    browseRoot: "/",
    path: "",
    items: [],             // raw listing
    byName: new Map(),
    vis: null,             // cached {items, index} after sort + filter
    sel: new Set(),
    cursor: -1,            // index into vis().items
    anchor: null,          // name anchoring shift-range selection
    hidden: false,
    loading: false,
    error: null,
    view: "list",
    filter: "",
    sort: { key: "name", dir: 1 },
    history: [],
    future: [],
    loadSeq: 0,
    el: null,
  };
}

const MAX_PANES = 6;

const state = {
  local: { name: "", reachable: true, os: "", path_style: "posix", case_insensitive: false, initial_root: "/", browse_root: "/" },
  servers: [],
  panes: [],
  active: 0,
  view: "files",
  clipboard: null,
  mobile: false,
  rowH: 34,
};

function activePane() { return state.panes[state.active] || state.panes[0]; }
function paneNumber(p) { return state.panes.indexOf(p) + 1; }

// other(p): the next pane (wrapping) — the default Copy/Move destination.
function other(p) {
  if (state.panes.length < 2) return null;
  return state.panes[(state.panes.indexOf(p) + 1) % state.panes.length];
}

function sourceInfo(server) {
  if (!server) return state.local;
  return state.servers.find((source) => source.name === server) || null;
}

function sourceName(server) { return server || "Local"; }
function locationLabel(server, path) { return sourceName(server) + ":" + path; }

function applyPaneSource(p, server, opts = {}) {
  const source = sourceInfo(server) || state.local;
  p.server = server || "";
  p.os = source.os || "";
  p.pathStyle = normalizePathStyle(source.path_style);
  p.caseInsensitive = Boolean(source.case_insensitive || p.pathStyle === "windows");
  p.initialRoot = source.initial_root || (p.pathStyle === "windows" ? "C:\\" : "/");
  p.browseRoot = source.browse_root || (p.pathStyle === "windows" ? "C:\\" : "/");
  if (!pathWithin(p.browseRoot, p.initialRoot, p.pathStyle)) p.initialRoot = p.browseRoot;
  // Remember where each source was last left, so switching back resumes there.
  const remembered = (store.get("lastPath", {}) || {})[p.server];
  let start = opts.path || remembered || p.initialRoot;
  if (!pathWithin(p.browseRoot, start, p.pathStyle)) start = p.initialRoot;
  p.path = cleanPath(start, p.pathStyle);
  p._restored = start !== p.initialRoot;
  p.history = [];
  p.future = [];
  p.filter = "";
  // Entries of the previous source must never be acted on under the new one.
  if (p.el) clearListing(p);
  else {
    p.items = [];
    p.byName = new Map();
    p.sel.clear();
    p.cursor = -1;
    p.anchor = null;
    p.vis = null;
  }
}

// ----------------------------------------------------------------- persistence

const saveLayout = debounce(() => {
  store.set("layout", {
    panes: state.panes.map((p) => ({ server: p.server, path: p.path, view: p.view, hidden: p.hidden, sort: p.sort })),
    active: state.active,
  });
}, 300);

function rememberPath(p) {
  const all = store.get("lastPath", {}) || {};
  all[p.server] = p.path;
  store.set("lastPath", all);
  const recent = (store.get("recent", []) || []).filter((r) => !(r.server === p.server && r.path === p.path));
  recent.unshift({ server: p.server, path: p.path });
  store.set("recent", recent.slice(0, 20));
}

// ----------------------------------------------------------------- toasts

// toast(message, kind, {action: {label, run}, duration})
function toast(message, kind = "", opts = {}) {
  const host = $("#toasts");
  const isError = kind === "error";
  const el = h("div", { class: "toast" + (kind ? " " + kind : ""), role: isError ? "alert" : "status" });
  const ico = kind === "success" ? "check" : isError ? "alert" : "info";
  el.append(icon(ico, "t-ico"), h("span", { class: "t-msg", text: message }));
  let timer = 0;
  const dismiss = () => {
    clearTimeout(timer);
    if (!el.isConnected) return;
    el.classList.add("leaving");
    setTimeout(() => el.remove(), reducedMotion() ? 0 : 200);
  };
  if (opts.action) {
    el.append(h("button", {
      class: "t-action", type: "button", text: opts.action.label,
      onclick: () => { dismiss(); opts.action.run(); },
    }));
  }
  el.append(h("button", { class: "t-close", type: "button", "aria-label": "Dismiss", onclick: dismiss }, icon("x")));
  const duration = opts.duration || (opts.action ? 8000 : isError ? 7000 : 4000);
  const arm = () => { clearTimeout(timer); timer = setTimeout(dismiss, duration); };
  el.addEventListener("pointerenter", () => clearTimeout(timer));
  el.addEventListener("pointerleave", arm);
  el.addEventListener("focusin", () => clearTimeout(timer));
  el.addEventListener("focusout", arm);
  host.append(el);
  while (host.children.length > 4) host.firstElementChild.remove();
  arm();
  return dismiss;
}

// ----------------------------------------------------------------- modal dialogs

const modalState = { onClose: null, lastFocus: null, onKey: null };

function focusables(root) {
  return $$('button:not([disabled]), [href], input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])', root)
    .filter((el) => el.offsetParent !== null || el === document.activeElement);
}

function modalOpen() { return !$("#modal-overlay").hidden; }

// showModal renders nodes into the shared dialog and traps focus inside it.
function showModal(nodes, { className = "", labelledBy = "", describedBy = "", onClose = null, onKey = null, initialFocus = null } = {}) {
  closeMenu(false);
  const overlay = $("#modal-overlay");
  const box = $("#modal");
  if (overlay.hidden) modalState.lastFocus = document.activeElement;
  box.className = "modal" + (className ? " " + className : "");
  box.replaceChildren(...[].concat(nodes));
  if (labelledBy) box.setAttribute("aria-labelledby", labelledBy); else box.removeAttribute("aria-labelledby");
  if (describedBy) box.setAttribute("aria-describedby", describedBy); else box.removeAttribute("aria-describedby");
  modalState.onClose = onClose;
  modalState.onKey = onKey;
  overlay.hidden = false;
  const target = initialFocus || focusables(box)[0];
  if (target) target.focus();
}

function closeModal(result) {
  const overlay = $("#modal-overlay");
  if (overlay.hidden) return;
  overlay.hidden = true;
  $("#modal").replaceChildren();
  const cb = modalState.onClose;
  modalState.onClose = null;
  modalState.onKey = null;
  const back = modalState.lastFocus;
  modalState.lastFocus = null;
  if (back && back.isConnected && typeof back.focus === "function") back.focus();
  if (cb) cb(result);
}

function setupModal() {
  const overlay = $("#modal-overlay");
  overlay.addEventListener("pointerdown", (e) => { if (e.target === overlay) closeModal(null); });
  overlay.addEventListener("keydown", (e) => {
    if (modalState.onKey && modalState.onKey(e)) return;
    if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closeModal(null); return; }
    if (e.key === "Tab") {
      const f = focusables($("#modal"));
      if (!f.length) return;
      const first = f[0], last = f[f.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
  });
}

let dialogSeq = 0;

// dialog shows a title, body nodes and action buttons; resolves with the
// clicked action's value (null on cancel/escape).
function dialog({ title, body = [], actions = [], className = "", initialFocus = null, onKey = null }) {
  return new Promise((resolve) => {
    const id = "dlg-" + ++dialogSeq;
    const heading = h("h2", { id, text: title });
    const buttons = actions.map((a) => h("button", {
      type: "button",
      class: "btn" + (a.kind ? " " + a.kind : ""),
      text: a.label,
      disabled: a.disabled || false,
      onclick: () => {
        if (a.validate && !a.validate()) return;
        closeModal(typeof a.value === "function" ? a.value() : a.value);
      },
    }));
    const bar = h("div", { class: "modal-actions" }, buttons);
    showModal([heading, ...[].concat(body), bar], {
      className, labelledBy: id, onClose: resolve, onKey,
      initialFocus: typeof initialFocus === "function" ? initialFocus(buttons) : initialFocus || buttons[buttons.length - 1],
    });
  });
}

function itemList(items, notes = {}) {
  const max = 60;
  const list = h("ul", { class: "item-list" });
  for (const it of items.slice(0, max)) {
    const note = notes[it.name];
    list.append(h("li", null,
      icon(iconNameFor(it), it.is_dir ? "dir" : ""),
      h("span", { class: "nm truncate", text: it.name, title: it.name }),
      note ? h("span", { class: "note" + (note.warn ? " warn" : ""), text: note.text }) :
        h("span", { class: "note", text: it.is_dir ? "folder" : humanSize(it.size) })));
  }
  if (items.length > max) list.append(h("li", null, h("span", { class: "nm muted", text: "…and " + (items.length - max) + " more" })));
  return list;
}

function callout(kind, iconName, ...content) {
  return h("div", { class: "callout " + kind }, icon(iconName), h("div", null, ...content));
}

function confirmDialog({ title, message, items = null, notes = {}, extra = [], okLabel = "OK", danger = false }) {
  const body = [];
  if (message) body.push(h("p", { class: "modal-desc" }, message));
  if (items) body.push(itemList(items, notes));
  body.push(...extra);
  return dialog({
    title, body,
    actions: [
      { label: "Cancel", value: false },
      { label: okLabel, value: true, kind: danger ? "danger-solid" : "primary" },
    ],
    // A destructive dialog starts on Cancel so a stray Enter can't confirm it.
    initialFocus: danger ? (buttons) => buttons[0] : null,
  }).then((v) => v === true);
}

// promptDialog asks for one value with live validation; resolves to the
// string or null.
function promptDialog({ title, message = "", value = "", okLabel = "OK", validate = null, selectStem = true, mono = false, hint = "", label = "" }) {
  const inputId = "dlg-in-" + ++dialogSeq;
  const errId = inputId + "-err";
  const input = h("input", { id: inputId, class: "text-input" + (mono ? " mono" : ""), type: "text", spellcheck: "false", autocomplete: "off", autocapitalize: "off", "aria-describedby": errId, value });
  const err = h("div", { id: errId, class: "field-error", "aria-live": "polite" });
  const fieldKids = [];
  if (label) fieldKids.push(h("label", { for: inputId, class: "mf-label", text: label }));
  fieldKids.push(input);
  if (hint) fieldKids.push(h("div", { class: "field-hint", text: hint }));
  fieldKids.push(err);
  const check = () => {
    const msg = validate ? validate(input.value) : (input.value.trim() ? "" : "A value is required");
    err.textContent = msg || "";
    input.setAttribute("aria-invalid", msg ? "true" : "false");
    return !msg;
  };
  input.addEventListener("input", debounce(check, 120));
  const body = [];
  if (message) body.push(h("p", { class: "modal-desc" }, message));
  body.push(h("div", { class: "modal-field" }, fieldKids));
  const p = dialog({
    title, body,
    actions: [
      { label: "Cancel", value: null },
      { label: okLabel, kind: "primary", validate: check, value: () => input.value },
    ],
    initialFocus: input,
    onKey: (e) => {
      if (e.key === "Enter" && e.target === input) {
        e.preventDefault();
        if (check()) closeModal(input.value);
        return true;
      }
      return false;
    },
  });
  const dot = value.lastIndexOf(".");
  if (selectStem && dot > 0) input.setSelectionRange(0, dot); else input.select();
  return p;
}

// ----------------------------------------------------------------- menus

const menuState = { invoker: null, onClose: null };

function menuOpen() { return !$("#menu").hidden; }

// openMenu shows a role=menu at (x, y) or under an anchor element. items are
// {label, icon, run, danger, disabled, hint} | "sep" | {title}.
function openMenu(items, { x = 0, y = 0, anchor = null, label = "Actions", onClose = null } = {}) {
  closeMenu(false);
  const menu = $("#menu");
  menu.replaceChildren();
  menu.setAttribute("aria-label", label);
  for (const it of items) {
    if (!it) continue;
    if (it === "sep") { menu.append(h("div", { class: "menu-sep", role: "separator" })); continue; }
    if (it.title) { menu.append(h("div", { class: "menu-title", text: it.title, role: "presentation" })); continue; }
    const b = h("button", { class: "menu-item" + (it.danger ? " danger" : ""), type: "button", role: "menuitem", tabindex: "-1", disabled: !!it.disabled });
    b.append(icon(it.icon || "cursor"), h("span", { class: "mi-label", text: it.label }));
    if (!it.icon) b.firstChild.style.visibility = "hidden";
    if (it.hint) b.append(h("span", { class: "mi-hint", text: it.hint }));
    if (!it.disabled) b.addEventListener("click", () => { closeMenu(true); it.run(); });
    menu.append(b);
  }
  menuState.invoker = anchor || document.activeElement;
  menuState.onClose = onClose;
  if (anchor) anchor.setAttribute("aria-expanded", "true");
  menu.hidden = false;
  let px = x, py = y;
  if (anchor) {
    const r = anchor.getBoundingClientRect();
    px = r.left;
    py = r.bottom + 4;
  }
  const mr = menu.getBoundingClientRect();
  if (anchor && px + mr.width > window.innerWidth - 8) px = anchor.getBoundingClientRect().right - mr.width;
  if (px + mr.width > window.innerWidth - 8) px = window.innerWidth - mr.width - 8;
  if (py + mr.height > window.innerHeight - 8) py = anchor ? anchor.getBoundingClientRect().top - mr.height - 4 : window.innerHeight - mr.height - 8;
  menu.style.left = Math.max(8, px) + "px";
  menu.style.top = Math.max(8, py) + "px";
  const first = $(".menu-item:not(:disabled)", menu);
  if (first) first.focus();
}

function closeMenu(restoreFocus = true) {
  const menu = $("#menu");
  if (menu.hidden) return;
  menu.hidden = true;
  menu.replaceChildren();
  const inv = menuState.invoker;
  menuState.invoker = null;
  if (inv && inv.getAttribute && inv.getAttribute("aria-expanded") === "true") inv.setAttribute("aria-expanded", "false");
  const cb = menuState.onClose;
  menuState.onClose = null;
  if (restoreFocus && inv && inv.isConnected && typeof inv.focus === "function") inv.focus();
  if (cb) cb();
}

function setupMenu() {
  const menu = $("#menu");
  menu.addEventListener("keydown", (e) => {
    const items = $$(".menu-item:not(:disabled)", menu);
    const idx = items.indexOf(document.activeElement);
    const go = (i) => { if (items.length) items[(i + items.length) % items.length].focus(); };
    if (e.key === "ArrowDown") { e.preventDefault(); go(idx + 1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); go(idx - 1); }
    else if (e.key === "Home") { e.preventDefault(); go(0); }
    else if (e.key === "End") { e.preventDefault(); go(items.length - 1); }
    else if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closeMenu(true); }
    else if (e.key === "Tab") { e.preventDefault(); closeMenu(true); }
    else if (e.key.length === 1 && /\S/.test(e.key)) {
      const ch = e.key.toLowerCase();
      const start = idx + 1;
      for (let k = 0; k < items.length; k++) {
        const it = items[(start + k) % items.length];
        if (it.textContent.trim().toLowerCase().startsWith(ch)) { it.focus(); break; }
      }
    }
  });
  document.addEventListener("pointerdown", (e) => {
    if (!menu.hidden && !e.target.closest("#menu")) closeMenu(false);
  }, true);
  window.addEventListener("resize", () => closeMenu(false));
  window.addEventListener("blur", () => closeMenu(false));
}

// ----------------------------------------------------------------- panes

function buildPanes() {
  const host = $("#panes");
  host.replaceChildren();
  const tpl = $("#pane-template");
  for (const p of state.panes) {
    const node = tpl.content.firstElementChild.cloneNode(true);
    host.appendChild(node);
    p.el = {
      root: node,
      source: $(".source", node),
      sourceIcon: $(".source-icon", node),
      server: $(".source-select", node),
      back: $(".nav-back", node),
      fwd: $(".nav-fwd", node),
      up: $(".nav-up", node),
      crumbs: $(".crumbs", node),
      crumbList: $(".crumb-list", node),
      pathInput: $(".path-input", node),
      refresh: $(".act-refresh", node),
      paneMenu: $(".act-pane-menu", node),
      close: $(".act-close", node),
      filterInput: $(".filter-input", node),
      newFolder: $(".tool-newfolder", node),
      upload: $(".tool-upload", node),
      hiddenToggle: $(".tool-hidden", node),
      viewToggle: $(".tool-view", node),
      previewToggle: $(".tool-preview", node),
      fileInput: $(".file-input", node),
      selbar: $(".selbar", node),
      selCount: $(".sel-count", node),
      selClear: $(".sel-clear", node),
      actDownload: $(".act-download", node),
      actCopy: $(".act-copy", node),
      actMove: $(".act-move", node),
      actCompress: $(".act-compress", node),
      actRename: $(".act-rename", node),
      actDelete: $(".act-delete", node),
      actMore: $(".act-more", node),
      listing: $(".listing", node),
      head: $(".list-head", node),
      grid: $(".grid", node),
      spacer: $(".grid-spacer", node),
      state: $(".grid-state", node),
      status: $(".status-text", node),
      statusExtra: $(".status-extra", node),
      dropDest: $(".drop-dest", node),
      pool: new Map(),
    };
    node.dataset.uid = String(p.uid);
    p.el.grid.id = "grid-" + p.uid;
    populateServerSelect(p);
    wirePane(p);
    applyViewClass(p);
    renderSort(p);
    renderNav(p);
    renderCrumbs(p);
    p.el.filterInput.value = p.filter;
    p.el.hiddenToggle.setAttribute("aria-pressed", String(p.hidden));
    setIcon(p.el.hiddenToggle.querySelector("svg"), p.hidden ? "eye" : "eye-off");
    if (p.items.length || p.error) { renderRows(p); renderSelectionInfo(p); if (p.error) showListError(p, p.error); }
  }
  host.dataset.count = String(state.panes.length);
  updatePaneChrome();
  setActivePane(activePane(), { focus: false });
}

// populateServerSelect fills a pane's source dropdown (Local + servers).
function populateServerSelect(p) {
  const sel = p.el.server;
  sel.replaceChildren(h("option", { value: "", text: "Local" }));
  for (const s of state.servers) {
    sel.append(h("option", { value: s.name, text: s.reachable ? s.name : s.name + " — offline", disabled: !s.reachable && s.name !== p.server }));
  }
  sel.value = p.server;
  renderSourceChrome(p);
}

function renderSourceChrome(p) {
  const r = p.el;
  if (!r) return;
  const src = sourceInfo(p.server);
  setIcon(r.sourceIcon, p.server ? "server" : "laptop");
  r.source.classList.toggle("offline", !!(p.server && src && !src.reachable));
  r.server.title = p.server ? p.server + (src && src.os ? " (" + src.os + ")" : "") : "This machine (the controller)";
  r.root.setAttribute("aria-label", "Pane " + paneNumber(p) + ": " + sourceName(p.server) + " " + p.path);
  r.grid.setAttribute("aria-label", "Files in " + locationLabel(p.server, p.path));
  r.dropDest.textContent = locationLabel(p.server, p.path);
}

function updatePaneChrome() {
  const single = state.panes.length <= 1;
  for (const p of state.panes) {
    if (!p.el) continue;
    p.el.close.disabled = single;
    p.el.close.hidden = single;
    renderSourceChrome(p);
  }
  renderPaneSwitcher();
}

function setActivePane(p, { focus = false } = {}) {
  if (!p) return;
  const idx = state.panes.indexOf(p);
  if (idx < 0) return;
  const changed = state.active !== idx;
  state.active = idx;
  for (const q of state.panes) {
    if (!q.el) continue;
    q.el.root.classList.toggle("active", q === p);
    q.el.root.classList.toggle("is-current", q === p);
  }
  if (changed) { renderPaneSwitcher(); saveLayout(); }
  document.body.classList.toggle("has-selbar", state.mobile && p.sel.size > 0);
  if (focus && p.el) p.el.grid.focus({ preventScroll: true });
  if (changed && state.mobile && p.el) requestAnimationFrame(() => renderRows(p));
}

function renderPaneSwitcher() {
  const host = $("#pane-switcher");
  if (!host) return;
  host.replaceChildren();
  state.panes.forEach((p, i) => {
    const on = i === state.active;
    host.append(h("button", {
      class: "ps-tab", type: "button", role: "tab",
      "aria-selected": String(on), tabindex: on ? "0" : "-1",
      "aria-controls": p.el ? p.el.grid.id : null,
      title: locationLabel(p.server, p.path),
      onclick: () => setActivePane(p, { focus: true }),
    }, icon(p.server ? "server" : "laptop"), h("span", { text: (i + 1) + " · " + sourceName(p.server) })));
  });
  if (state.panes.length < MAX_PANES) {
    host.append(h("button", { class: "ps-add", type: "button", "aria-label": "Add pane", title: "Add pane", onclick: addPane }, icon("plus")));
  }
  host.onkeydown = (e) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    const tabs = $$(".ps-tab", host);
    const i = tabs.indexOf(document.activeElement);
    if (i < 0) return;
    const next = tabs[(i + (e.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length];
    next.focus();
    next.click();
  };
}

function addPane(server) {
  if (state.panes.length >= MAX_PANES) {
    toast("You can have up to " + MAX_PANES + " panes", "error");
    return null;
  }
  let target = typeof server === "string" ? server : undefined;
  if (target === undefined) {
    const used = new Set(state.panes.map((p) => p.server));
    const reachable = state.servers.filter((s) => s.reachable);
    const unused = reachable.find((s) => !used.has(s.name));
    target = unused ? unused.name : reachable[0] ? reachable[0].name : "";
  }
  const next = newPaneState(target);
  applyPaneSource(next, target);
  state.panes.push(next);
  state.active = state.panes.length - 1;
  buildPanes();
  setActivePane(next, { focus: true });
  loadListing(next);
  saveLayout();
  return next;
}

function removePane(p) {
  if (state.panes.length <= 1) return;
  const idx = state.panes.indexOf(p);
  if (idx < 0) return;
  state.panes.splice(idx, 1);
  if (state.active >= state.panes.length) state.active = state.panes.length - 1;
  if (preview.p === p) closePreview();
  buildPanes();
  setActivePane(activePane(), { focus: true });
  saveLayout();
}

function wirePane(p) {
  const r = p.el;
  r.root.addEventListener("pointerdown", () => setActivePane(p), true);
  r.root.addEventListener("focusin", () => setActivePane(p));

  r.server.addEventListener("change", (e) => {
    setActivePane(p);
    applyPaneSource(p, e.target.value);
    r.filterInput.value = "";
    renderSourceChrome(p);
    loadListing(p);
    saveLayout();
  });

  r.back.addEventListener("click", () => goBack(p));
  r.fwd.addEventListener("click", () => goForward(p));
  r.up.addEventListener("click", () => goUp(p));
  r.refresh.addEventListener("click", () => loadListing(p, { keepScroll: true }));
  r.close.addEventListener("click", (e) => { e.stopPropagation(); removePane(p); });
  r.paneMenu.addEventListener("click", () => openPaneMenu(p, r.paneMenu));
  r.newFolder.addEventListener("click", () => promptMkdir(p));
  r.upload.addEventListener("click", () => r.fileInput.click());
  r.fileInput.addEventListener("change", (e) => {
    if (e.target.files.length) uploadFiles(p, Array.from(e.target.files));
    e.target.value = "";
  });
  r.hiddenToggle.addEventListener("click", () => toggleHidden(p));
  r.viewToggle.addEventListener("click", () => setView(p, p.view === "list" ? "icons" : "list"));
  r.previewToggle.addEventListener("click", () => togglePreview(p));

  // Location: crumbs, or an editable path on click / Ctrl+L / G.
  r.crumbs.addEventListener("click", (e) => { if (e.target === r.crumbs || e.target === r.crumbList) startPathEdit(p); });
  r.pathInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); commitPathEdit(p); }
    else if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); endPathEdit(p, true); }
  });
  r.pathInput.addEventListener("blur", () => endPathEdit(p, false));

  // Filter (debounced for large folders).
  const applyFilter = () => {
    p.filter = r.filterInput.value;
    invalidate(p);
    p.cursor = vis(p).items.length ? 0 : -1;
    r.grid.scrollTop = 0;
    renderRows(p);
    renderSelectionInfo(p);
  };
  const applyFilterDebounced = debounce(applyFilter, 120);
  r.filterInput.addEventListener("input", () => (p.items.length > 3000 ? applyFilterDebounced() : applyFilter()));
  r.filterInput.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      if (r.filterInput.value) { r.filterInput.value = ""; applyFilter(); }
      else r.grid.focus();
    } else if (e.key === "ArrowDown" || e.key === "Enter") {
      e.preventDefault();
      applyFilterDebounced.cancel();
      applyFilter();
      if (vis(p).items.length) { selectOnly(p, 0); }
      r.grid.focus();
    }
  });

  for (const col of $$(".col", r.head)) col.addEventListener("click", () => setSort(p, col.dataset.sort));

  // Selection actions.
  r.selClear.addEventListener("click", () => { clearSelection(p); r.grid.focus(); });
  r.actDownload.addEventListener("click", () => downloadSelection(p));
  r.actCopy.addEventListener("click", () => transferDialog(p, "copy"));
  r.actMove.addEventListener("click", () => transferDialog(p, "move"));
  r.actCompress.addEventListener("click", () => actCompress(p));
  r.actRename.addEventListener("click", () => actRename(p));
  r.actDelete.addEventListener("click", () => actDelete(p));
  r.actMore.addEventListener("click", () => {
    const items = selectedItems(p);
    openMenu(itemMenuItems(p, items[0], items), { anchor: r.actMore, label: "Selection actions" });
  });

  // The virtual grid: delegated pointer + keyboard handling.
  let raf = 0;
  r.grid.addEventListener("scroll", () => {
    if (raf) return;
    raf = requestAnimationFrame(() => { raf = 0; renderRows(p); });
  }, { passive: true });
  r.grid.addEventListener("keydown", (e) => onGridKey(p, e));
  r.grid.addEventListener("pointerdown", (e) => onGridPointerDown(p, e));
  r.grid.addEventListener("click", (e) => onGridClick(p, e));
  r.grid.addEventListener("dblclick", (e) => onGridDblClick(p, e));
  r.grid.addEventListener("contextmenu", (e) => onGridContext(p, e));
  // Full timestamps are formatted lazily, only for the cell being hovered.
  r.grid.addEventListener("pointerover", (e) => {
    const cell = e.target.closest && e.target.closest(".c-mod");
    if (!cell || cell.title) return;
    const row = cell.closest("[data-idx]");
    const it = row && vis(p).items[+row.dataset.idx];
    if (it && it.mod_time) cell.title = fmtDateFull(it.mod_time);
  });
  r.grid.addEventListener("focus", () => {
    if (p.cursor < 0 && vis(p).items.length) setCursor(p, 0, { scroll: false });
    else paintSelection(p);
  });
  if (window.ResizeObserver) {
    let lastW = 0, lastH = 0;
    new ResizeObserver((entries) => {
      const cr = entries[0].contentRect;
      if (Math.abs(cr.width - lastW) < 1 && Math.abs(cr.height - lastH) < 1) return;
      lastW = cr.width; lastH = cr.height;
      renderRows(p);
      if (crumbRoom(p) !== p._crumbRoom) renderCrumbs(p);
    }).observe(r.grid);
  }
  setupFileDropZone(p);
}

function openPaneMenu(p, anchor) {
  const idx = state.panes.indexOf(p);
  openMenu([
    { title: "Pane " + (idx + 1) + " · " + locationLabel(p.server, p.path) },
    { label: "New folder", icon: "folder-plus", run: () => promptMkdir(p) },
    { label: "New file", icon: "file-plus", run: () => promptNewFile(p) },
    { label: "Upload files…", icon: "upload", run: () => p.el.fileInput.click() },
    { label: "Paste", icon: "clipboard", hint: MOD + "+V", disabled: !state.clipboard, run: () => pasteInto(p) },
    "sep",
    { label: "Go to path…", icon: "arrow-right", hint: "G", run: () => startPathEdit(p) },
    { label: "Go to start folder", icon: "home", run: () => navigate(p, p.initialRoot) },
    { label: p.hidden ? "Hide hidden files" : "Show hidden files", icon: p.hidden ? "eye-off" : "eye", hint: ".", run: () => toggleHidden(p) },
    { label: p.view === "list" ? "Icon view" : "List view", icon: p.view === "list" ? "grid" : "list", hint: "V", run: () => setView(p, p.view === "list" ? "icons" : "list") },
    { label: "Select all", icon: "check", hint: MOD + "+A", run: () => selectAll(p) },
    { label: "Copy folder path", icon: "copy", run: () => copyText(p.path).then((ok) => toast(ok ? "Copied " + p.path : p.path, ok ? "success" : "")) },
    "sep",
    { label: "Add pane", icon: "columns", disabled: state.panes.length >= MAX_PANES, run: () => addPane() },
    { label: "Close pane", icon: "x", disabled: state.panes.length <= 1, run: () => removePane(p) },
  ], { anchor, label: "Pane options" });
}

// ----------------------------------------------------------------- navigation

function navigate(p, path, opts = {}) {
  path = cleanPath(path, p.pathStyle);
  if (path === p.path && !opts.force) return;
  if (opts.push !== false && p.path) {
    p.history.push(p.path);
    if (p.history.length > 100) p.history.shift();
    p.future = [];
  }
  p.path = path;
  clearListing(p);
  p.filter = "";
  if (p.el) p.el.filterInput.value = "";
  loadListing(p, { focusName: opts.focusName });
}

// clearListing drops the entries of the previous location immediately, so no
// action (Delete, F2, Enter…) can ever pair an old entry name with the new path
// while the new listing is still in flight.
function clearListing(p) {
  p.items = [];
  p.byName = new Map();
  p.sel.clear();
  p.cursor = -1;
  p.anchor = null;
  p.error = null;
  invalidate(p);
  if (p.el) {
    renderRows(p);
    renderSelectionInfo(p);
  }
}

function goUp(p) {
  const parent = parentPath(p.path, p.pathStyle, p.browseRoot);
  if (parent === p.path) return;
  navigate(p, parent, { focusName: baseName(p.path, p.pathStyle) });
}

function goBack(p) {
  if (!p.history.length) return;
  const prev = p.history.pop();
  p.future.push(p.path);
  navigate(p, prev, { push: false, focusName: baseName(p.path, p.pathStyle) });
}

function goForward(p) {
  if (!p.future.length) return;
  const next = p.future.pop();
  p.history.push(p.path);
  navigate(p, next, { push: false });
}

function renderNav(p) {
  if (!p.el) return;
  p.el.back.disabled = !p.history.length;
  p.el.fwd.disabled = !p.future.length;
  p.el.up.disabled = parentPath(p.path || p.initialRoot, p.pathStyle, p.browseRoot) === p.path;
}

// refreshPanesAt reloads every pane currently showing dir on server (used when
// a transfer, upload or mutation finishes — regardless of which pane started it).
function refreshPanesAt(server, dir, style) {
  for (const p of state.panes) {
    if (p.server !== (server || "")) continue;
    if (dir === undefined || cleanPath(p.path, p.pathStyle) === cleanPath(dir, style || p.pathStyle)) {
      loadListing(p, { keepScroll: true, quiet: true });
    }
  }
}

function setLoading(p, on) {
  p.loading = on;
  if (!p.el) return;
  if (on) p.el.root.dataset.loading = "true";
  else delete p.el.root.dataset.loading;
  p.el.grid.setAttribute("aria-busy", String(on));
}

async function loadListing(p, opts = {}) {
  const seq = ++p.loadSeq;
  setLoading(p, true);
  renderNav(p);
  renderCrumbs(p);
  renderSourceChrome(p);
  const spinnerTimer = opts.quiet ? 0 : setTimeout(() => {
    if (p.loadSeq === seq && p.loading) showGridState(p, "loading", { title: "Loading…" });
  }, 180);
  const scrollTop = p.el ? p.el.grid.scrollTop : 0;
  let result;
  try {
    const params = { server: p.server, path: p.path };
    if (p.hidden) params.hidden = "1";
    result = await getJSON("/api/list", params);
  } catch (e) {
    clearTimeout(spinnerTimer);
    if (seq !== p.loadSeq) return;
    setLoading(p, false);
    // A remembered path that no longer works falls back to the start folder.
    if (p._restored && p.path !== p.initialRoot) {
      p._restored = false;
      p.path = p.initialRoot;
      loadListing(p);
      return;
    }
    p.error = e;
    p.items = [];
    p.byName = new Map();
    invalidate(p);
    renderRows(p);
    renderSelectionInfo(p);
    showListError(p, e);
    return;
  }
  clearTimeout(spinnerTimer);
  if (seq !== p.loadSeq) return;
  p._restored = false;
  setLoading(p, false);
  p.error = null;
  p.path = result.path || p.path;
  if (typeof result.case_insensitive === "boolean") p.caseInsensitive = result.case_insensitive;
  const entries = result.entries || [];
  for (const it of entries) it._lc = it.name.toLowerCase();
  p.items = entries;
  p.byName = new Map(entries.map((it) => [it.name, it]));
  for (const n of Array.from(p.sel)) if (!p.byName.has(n)) p.sel.delete(n);
  invalidate(p);
  hideGridState(p);
  renderCrumbs(p);
  renderNav(p);
  renderSourceChrome(p);
  if (opts.keepScroll && p.el) p.el.grid.scrollTop = scrollTop;
  else if (p.el) p.el.grid.scrollTop = 0;
  if (opts.focusName) {
    const i = vis(p).index.get(opts.focusName);
    if (i !== undefined) { p.cursor = i; p.anchor = opts.focusName; }
  } else if (p.cursor >= vis(p).items.length) {
    p.cursor = vis(p).items.length - 1;
  }
  renderRows(p);
  if (p.cursor >= 0) scrollToIndex(p, p.cursor);
  renderSelectionInfo(p);
  if (!entries.length) showGridState(p, "empty", { title: "This folder is empty", text: "Drop files here or use Upload to add some.", icon: "folder" });
  rememberPath(p);
  saveLayout();
  if (preview.open && preview.p === p) syncPreviewToCursor(p);
}

function showListError(p, e) {
  const msg = String((e && e.message) || e);
  const actions = [];
  let title = "Couldn't open this folder";
  let kind = "error";
  let ico = "alert";
  if (/outside the agent's allowed file roots|is not permitted/i.test(msg)) {
    title = "Outside the allowed file roots";
    kind = "warn";
    ico = "lock";
    actions.push({ label: "Go to allowed root", primary: true, run: () => navigate(p, p.browseRoot) });
  } else if (/configuration directory/i.test(msg)) {
    title = "Protected folder";
    kind = "warn";
    ico = "lock";
  }
  if (p.history.length) actions.push({ label: "Go back", run: () => goBack(p) });
  if (p.path !== p.initialRoot && !actions.length) actions.push({ label: "Go to start folder", run: () => navigate(p, p.initialRoot) });
  actions.push({ label: "Retry", run: () => loadListing(p) });
  showGridState(p, kind, { title, text: friendlyError(e, p.server) + (friendlyError(e, p.server) !== msg ? " (" + msg + ")" : ""), icon: ico, actions });
}

function showGridState(p, kind, { title = "", text = "", icon: ico = "", actions = [] } = {}) {
  if (!p.el) return;
  const box = p.el.state;
  box.className = "grid-state " + kind + (kind === "loading" ? " soft" : "");
  const kids = [];
  if (kind === "loading") kids.push(h("div", { class: "spinner", role: "progressbar", "aria-label": "Loading" }));
  else if (ico) kids.push(icon(ico, "state-ico"));
  if (title) kids.push(h("h3", { text: title }));
  if (text) kids.push(h("p", { text }));
  if (actions.length) {
    kids.push(h("div", { class: "state-actions" }, actions.map((a) => h("button", {
      class: "btn" + (a.primary ? " primary" : ""), type: "button", text: a.label, onclick: a.run,
    }))));
  }
  box.replaceChildren(...kids);
  box.hidden = false;
  box.setAttribute("role", kind === "error" || kind === "warn" ? "alert" : "status");
}

function hideGridState(p) {
  if (p.el) p.el.state.hidden = true;
}

// ----------------------------------------------------------------- breadcrumbs

// crumbRoom is how many trailing path segments fit beside the pane's controls.
function crumbRoom(p) {
  const loc = p.el && p.el.crumbs.parentElement;
  const room = loc && loc.clientWidth ? loc.clientWidth : 320;
  return room < 130 ? 1 : room < 230 ? 2 : room < 360 ? 3 : 5;
}

function renderCrumbs(p) {
  if (!p.el) return;
  const list = p.el.crumbList;
  list.replaceChildren();
  const path = p.path || p.initialRoot;
  const crumbs = pathBreadcrumbs(path, p.pathStyle, p.browseRoot);
  // Long paths collapse the middle segments into an overflow menu; how many
  // segments stay visible depends on the room this pane actually has.
  const maxVisible = crumbRoom(p);
  p._crumbRoom = maxVisible;
  let hiddenCrumbs = [];
  let shown = crumbs;
  if (crumbs.length > maxVisible + 1) {
    hiddenCrumbs = crumbs.slice(1, crumbs.length - maxVisible);
    shown = [crumbs[0], null, ...crumbs.slice(crumbs.length - maxVisible)];
  }
  shown.forEach((crumb, idx) => {
    const li = h("li", { class: idx === 0 || crumb === null ? "fixed" : "" });
    if (idx > 0) li.append(icon("chevron-right", "crumb-sep"));
    if (crumb === null) {
      const more = h("button", { class: "crumb overflow", type: "button", text: "…", "aria-label": "Show hidden path segments", "aria-haspopup": "menu" });
      more.addEventListener("click", (e) => {
        e.stopPropagation();
        openMenu(hiddenCrumbs.map((c) => ({ label: c.label, icon: "folder", run: () => navigate(p, c.path) })), { anchor: more, label: "Path segments" });
      });
      li.append(more);
    } else {
      const last = idx === shown.length - 1;
      // A long bounded root (e.g. an agent's --file-root) shows only its last
      // segment; the full path is in the tooltip and in the editable path.
      let label = crumb.label;
      if (idx === 0 && !last && label.length > 14) label = "…" + (p.pathStyle === "windows" ? "\\" : "/") + baseName(label, p.pathStyle);
      const b = h("button", {
        class: "crumb" + (last ? " current" : ""), type: "button", text: label, title: crumb.path,
        "aria-current": last ? "location" : null, dataset: { path: crumb.path },
      });
      b.addEventListener("click", (e) => {
        e.stopPropagation();
        if (last) startPathEdit(p);
        else navigate(p, crumb.path);
      });
      li.append(b);
    }
    list.append(li);
  });
}

function startPathEdit(p) {
  const r = p.el;
  if (!r) return;
  r.crumbs.hidden = true;
  r.pathInput.hidden = false;
  r.pathInput.value = p.path;
  r.pathInput.removeAttribute("aria-invalid");
  r.pathInput.focus();
  r.pathInput.select();
}

function endPathEdit(p, refocus) {
  const r = p.el;
  if (!r || r.pathInput.hidden) return;
  r.pathInput.hidden = true;
  r.crumbs.hidden = false;
  if (refocus) r.grid.focus();
}

function commitPathEdit(p) {
  const raw = p.el.pathInput.value.trim();
  if (!raw) { endPathEdit(p, true); return; }
  let target = raw;
  const absolute = p.pathStyle === "windows" ? /^([A-Za-z]:[\\/]|[\\/]{2})/.test(raw) : raw.startsWith("/");
  if (!absolute) target = joinPath(p.path, raw, p.pathStyle);
  target = cleanPath(target, p.pathStyle);
  if (!pathWithin(p.browseRoot, target, p.pathStyle)) {
    p.el.pathInput.setAttribute("aria-invalid", "true");
    toast("That path is outside " + sourceName(p.server) + "'s browsable root (" + p.browseRoot + ")", "error");
    return;
  }
  endPathEdit(p, true);
  navigate(p, target);
}

// ----------------------------------------------------------------- sort / filter

// computeVis orders the listing (folders first, then the pane's sort key,
// name as tiebreaker) and applies the case-insensitive filter. It is cached
// per pane until the listing, sort or filter changes, so scrolling and
// selection never re-sort 50k entries.
function computeVis(p) {
  const { key, dir } = p.sort;
  const items = p.items.slice();
  const byName = (a, b) => COLLATOR.compare(a.name, b.name);
  let cmp;
  if (key === "size") cmp = (a, b) => ((a.is_dir ? 0 : a.size || 0) - (b.is_dir ? 0 : b.size || 0)) || byName(a, b);
  else if (key === "mod") {
    for (const it of items) if (it._t === undefined) it._t = Date.parse(it.mod_time) || 0;
    cmp = (a, b) => (a._t - b._t) || byName(a, b);
  } else cmp = byName;
  items.sort((a, b) => {
    if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
    return cmp(a, b) * dir;
  });
  const f = p.filter.trim().toLowerCase();
  const out = f ? items.filter((it) => (it._lc || it.name.toLowerCase()).includes(f)) : items;
  const index = new Map();
  for (let i = 0; i < out.length; i++) index.set(out[i].name, i);
  return { items: out, index };
}

function vis(p) {
  if (!p.vis) p.vis = computeVis(p);
  return p.vis;
}

function invalidate(p) { p.vis = null; }

function setSort(p, key) {
  const cursorName = p.cursor >= 0 ? (vis(p).items[p.cursor] || {}).name : null;
  if (p.sort.key === key) p.sort.dir = -p.sort.dir;
  else { p.sort.key = key; p.sort.dir = key === "name" ? 1 : -1; }
  invalidate(p);
  if (cursorName) p.cursor = vis(p).index.get(cursorName) ?? -1;
  renderSort(p);
  renderRows(p);
  if (p.cursor >= 0) scrollToIndex(p, p.cursor);
  saveLayout();
  announce("Sorted by " + key + (p.sort.dir > 0 ? " ascending" : " descending"));
}

function renderSort(p) {
  if (!p.el) return;
  const names = { name: "Name", size: "Size", mod: "Modified" };
  for (const col of $$(".col", p.el.head)) {
    const active = col.dataset.sort === p.sort.key;
    if (active) col.setAttribute("aria-sort", p.sort.dir > 0 ? "ascending" : "descending");
    else col.removeAttribute("aria-sort");
    const dir = active ? (p.sort.dir > 0 ? "ascending" : "descending") : "";
    col.setAttribute("aria-label", "Sort by " + names[col.dataset.sort] + (dir ? " (currently " + dir + ")" : ""));
  }
}

function setView(p, view) {
  if (p.view === view) return;
  p.view = view;
  applyViewClass(p);
  if (p.el) p.el.mode = null; // force a full re-layout
  renderRows(p);
  if (p.cursor >= 0) scrollToIndex(p, p.cursor);
  saveLayout();
}

function applyViewClass(p) {
  if (!p.el) return;
  const icons = p.view === "icons";
  p.el.root.classList.toggle("view-icons", icons);
  p.el.root.classList.toggle("view-list", !icons);
  p.el.viewToggle.setAttribute("aria-pressed", String(icons));
  p.el.viewToggle.setAttribute("aria-label", icons ? "List view" : "Icon view");
  p.el.viewToggle.title = (icons ? "List view" : "Icon view") + " (V)";
  setIcon(p.el.viewToggle.querySelector("svg"), icons ? "list" : "grid");
}

function toggleHidden(p) {
  p.hidden = !p.hidden;
  if (p.el) {
    p.el.hiddenToggle.setAttribute("aria-pressed", String(p.hidden));
    p.el.hiddenToggle.setAttribute("aria-label", p.hidden ? "Hide hidden files" : "Show hidden files");
    setIcon(p.el.hiddenToggle.querySelector("svg"), p.hidden ? "eye" : "eye-off");
  }
  loadListing(p, { keepScroll: true });
  saveLayout();
  announce(p.hidden ? "Showing hidden files" : "Hiding hidden files");
}

// ----------------------------------------------------------------- virtual list

const ICON_CELL_MIN_W = 108;
const ICON_ROW_H = 118;
const OVERSCAN = 8;

function measureRowHeight() {
  const v = parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--row-h"));
  state.rowH = v > 0 ? v : 34;
}

function gridLayout(p) {
  const icons = p.view === "icons";
  if (!icons) return { icons, rowH: state.rowH, cols: 1 };
  const width = Math.max(0, (p.el.grid.clientWidth || 300) - 20);
  const cols = Math.max(1, Math.floor((width + 6) / (ICON_CELL_MIN_W + 6)));
  return { icons, rowH: ICON_ROW_H, cols };
}

// renderRows draws only the rows in (and just around) the viewport. List rows
// are recycled in place — a row leaving the window is re-filled for the one
// entering it, with no DOM creation or removal — and per-entry formatting is
// cached, so a 50k-entry folder scrolls with a few dozen stable nodes.
function renderRows(p) {
  const r = p.el;
  if (!r) return;
  const { items } = vis(p);
  const n = items.length;
  const lay = gridLayout(p);
  p._layout = lay;
  const rows = lay.icons ? Math.ceil(n / lay.cols) : n;
  const total = rows * lay.rowH;
  if (r.spacer._h !== total) { r.spacer.style.height = total + "px"; r.spacer._h = total; }
  r.grid.setAttribute("aria-rowcount", String(rows));
  r.grid.setAttribute("aria-colcount", String(lay.icons ? lay.cols : 3));
  const viewH = r.grid.clientHeight || 600;
  const top = r.grid.scrollTop;
  const first = Math.max(0, Math.floor(top / lay.rowH) - OVERSCAN);
  const last = Math.min(rows - 1, Math.ceil((top + viewH) / lay.rowH) + OVERSCAN);
  const mode = lay.icons ? "i" + lay.cols : "l";
  if (r.mode !== mode) {
    for (const el of r.pool.values()) el.remove();
    r.pool.clear();
    for (const el of r.free || []) el.remove();
    r.free = [];
    r.mode = mode;
  }
  if (!r.free) r.free = [];
  for (const [k, el] of r.pool) {
    if (k < first || k > last) {
      // Park the node: hidden, and stripped of anything that identifies an
      // entry (so hit-testing, aria-activedescendant and queries skip it).
      r.pool.delete(k);
      el.hidden = true;
      el._item = null;
      el._idx = -1;
      el._first = null;
      el.removeAttribute("id");
      el.removeAttribute("data-idx");
      el.removeAttribute("aria-selected");
      el.classList.remove("is-cursor", "cut", "dragging", "drop-into");
      if (el.classList.contains("vrow")) el.replaceChildren();
      r.free.push(el);
    }
  }
  for (let k = first; k <= last; k++) {
    let el = r.pool.get(k);
    if (!el) {
      el = r.free.pop();
      if (!el) {
        el = lay.icons ? makeVrow(lay) : makeRow();
        r.spacer.appendChild(el);
      }
      el.hidden = false;
      r.pool.set(k, el);
    }
    if (lay.icons) {
      const start = k * lay.cols;
      const slice = items.slice(start, start + lay.cols);
      if (el._first !== slice[0] || el._count !== slice.length || el._last !== slice[slice.length - 1]) {
        el._first = slice[0];
        el._last = slice[slice.length - 1];
        el._count = slice.length;
        el.setAttribute("aria-rowindex", String(k + 1));
        el.replaceChildren(...slice.map((it, j) => buildCell(p, it, start + j)));
      }
    } else if (el._item !== items[k] || el._idx !== k) {
      fillRow(p, el, items[k], k);
    }
    const y = k * lay.rowH;
    if (el._y !== y) { el.style.transform = "translateY(" + y + "px)"; el._y = y; }
  }
  paintSelection(p);
}

function makeVrow(lay) {
  const el = document.createElement("div");
  el.className = "vrow";
  el.setAttribute("role", "row");
  el.style.height = lay.rowH + "px";
  el.style.gridTemplateColumns = "repeat(" + lay.cols + ", minmax(0, 1fr))";
  return el;
}

// makeRow builds a list row skeleton once; fillRow only updates its content.
function makeRow() {
  const el = document.createElement("div");
  el.setAttribute("role", "row");
  const fi = icon("file", "fi");
  const nm = h("span", { class: "nm" });
  const sym = h("span", { class: "sym-tag", text: "link", hidden: true });
  const name = h("div", { class: "c-name", role: "gridcell" }, h("span", { class: "check", "aria-hidden": "true" }, icon("check")), fi, nm, sym);
  const size = h("div", { class: "c-size", role: "gridcell" });
  const mod = h("div", { class: "c-mod", role: "gridcell" });
  el.append(name, size, mod);
  el._r = { fi, use: fi.firstChild, nm, sym, size, mod };
  return el;
}

// entryView caches the per-entry display strings (formatting dates through
// Intl is by far the most expensive part of painting a row).
function entryView(item) {
  if (!item._v) {
    const k = kindOf(item);
    item._v = {
      kind: k,
      icon: KIND_ICON[k] || "file",
      size: item.is_dir ? "—" : humanSize(item.size),
      time: fmtTime(item.mod_time),
    };
  }
  return item._v;
}

function fillRow(p, el, item, idx) {
  const v = entryView(item);
  const r = el._r;
  el._item = item;
  el._idx = idx;
  el.className = "row " + v.kind;
  el.id = "p" + p.uid + "-" + idx;
  el.dataset.idx = String(idx);
  el.setAttribute("aria-rowindex", String(idx + 1));
  r.use.setAttribute("href", "#i-" + v.icon);
  r.fi.setAttribute("class", "i fi" + (item.is_dir ? " dir" : item.is_symlink ? " link" : ""));
  r.nm.textContent = item.name;
  r.nm.title = item.name;
  r.sym.hidden = !item.is_symlink;
  r.size.textContent = v.size;
  if (item.is_dir) r.size.setAttribute("aria-label", "Folder"); else r.size.removeAttribute("aria-label");
  r.mod.textContent = v.time;
  r.mod.removeAttribute("title"); // the full timestamp is filled in lazily on hover
}

function buildCell(p, item, idx) {
  const v = entryView(item);
  const cell = h("div", {
    class: "cell " + v.kind, role: "gridcell", id: "p" + p.uid + "-" + idx,
    dataset: { idx: String(idx) }, title: item.name,
  },
  h("span", { class: "check", "aria-hidden": "true" }, icon("check")),
  icon(v.icon, "fi " + (item.is_dir ? "dir" : item.is_symlink ? "link" : "")),
  h("span", { class: "nm", text: item.name }),
  h("span", { class: "meta", text: item.is_dir ? "Folder" : v.size }));
  cell._item = item;
  return cell;
}

// paintSelection updates selection/cursor state on the rendered nodes only.
function paintSelection(p) {
  const r = p.el;
  if (!r) return;
  const items = vis(p).items;
  const cut = state.clipboard && state.clipboard.mode === "cut" && state.clipboard.server === p.server && state.clipboard.dir === p.path ? state.clipboard.names : null;
  let activeId = "";
  const paint = (el, idx) => {
    const it = items[idx];
    if (!it) return;
    const selected = p.sel.has(it.name);
    if (el.getAttribute("aria-selected") !== String(selected)) el.setAttribute("aria-selected", String(selected));
    const isCursor = idx === p.cursor;
    el.classList.toggle("is-cursor", isCursor);
    el.classList.toggle("cut", !!(cut && cut.has(it.name)));
    if (isCursor) activeId = el.id;
  };
  for (const el of r.pool.values()) {
    if (el.classList.contains("vrow")) for (const cell of el.children) paint(cell, +cell.dataset.idx);
    else paint(el, el._idx);
  }
  if (activeId) r.grid.setAttribute("aria-activedescendant", activeId);
  else r.grid.removeAttribute("aria-activedescendant");
  r.grid.classList.toggle("selecting", p.sel.size > 0);
}

function scrollToIndex(p, idx) {
  const r = p.el;
  if (!r || idx < 0) return;
  const lay = p._layout || gridLayout(p);
  const row = lay.icons ? Math.floor(idx / lay.cols) : idx;
  const top = row * lay.rowH;
  const bottom = top + lay.rowH;
  const viewH = r.grid.clientHeight;
  if (top < r.grid.scrollTop) r.grid.scrollTop = top;
  else if (bottom > r.grid.scrollTop + viewH) r.grid.scrollTop = bottom - viewH;
  renderRows(p);
}

// ----------------------------------------------------------------- selection

function selectedItems(p) {
  const out = [];
  for (const name of p.sel) {
    const it = p.byName.get(name);
    if (it) out.push(it);
  }
  const index = vis(p).index;
  out.sort((a, b) => (index.get(a.name) ?? 0) - (index.get(b.name) ?? 0));
  return out;
}

function setCursor(p, idx, { scroll = true } = {}) {
  const n = vis(p).items.length;
  if (!n) { p.cursor = -1; return; }
  p.cursor = Math.max(0, Math.min(n - 1, idx));
  if (scroll) scrollToIndex(p, p.cursor);
  else paintSelection(p);
  if (preview.open && preview.p === p) syncPreviewToCursor(p);
}

function selectionChanged(p) {
  paintSelection(p);
  renderSelectionInfo(p);
}

function selectOnly(p, idx) {
  const it = vis(p).items[idx];
  if (!it) return;
  p.sel.clear();
  p.sel.add(it.name);
  p.anchor = it.name;
  setCursor(p, idx);
  selectionChanged(p);
}

function toggleOne(p, idx) {
  const it = vis(p).items[idx];
  if (!it) return;
  if (p.sel.has(it.name)) p.sel.delete(it.name); else p.sel.add(it.name);
  p.anchor = it.name;
  setCursor(p, idx);
  selectionChanged(p);
}

function selectRange(p, idx, additive) {
  const { items, index } = vis(p);
  const a = p.anchor !== null && index.has(p.anchor) ? index.get(p.anchor) : (p.cursor >= 0 ? p.cursor : idx);
  const [lo, hi] = a < idx ? [a, idx] : [idx, a];
  if (!additive) p.sel.clear();
  for (let k = lo; k <= hi; k++) p.sel.add(items[k].name);
  setCursor(p, idx);
  selectionChanged(p);
}

// selectAll toggles selection over the currently visible (filtered) set.
function selectAll(p) {
  const items = vis(p).items;
  const all = items.length > 0 && items.every((it) => p.sel.has(it.name));
  if (all) for (const it of items) p.sel.delete(it.name);
  else for (const it of items) p.sel.add(it.name);
  selectionChanged(p);
  announce(all ? "Selection cleared" : plural(items.length, "item") + " selected");
}

function clearSelection(p) {
  if (!p.sel.size) return;
  p.sel.clear();
  selectionChanged(p);
}

// renderSelectionInfo drives the selection bar (count + total size + actions)
// and the pane's status line.
function renderSelectionInfo(p) {
  const r = p.el;
  if (!r) return;
  const sel = selectedItems(p);
  const n = sel.length;
  r.root.classList.toggle("has-selection", n > 0);
  r.selbar.hidden = n === 0;
  if (p === activePane()) document.body.classList.toggle("has-selbar", state.mobile && n > 0);
  if (n) {
    const dirs = sel.filter((it) => it.is_dir).length;
    const bytes = sel.reduce((a, it) => a + (it.is_dir ? 0 : it.size || 0), 0);
    const parts = [];
    if (n - dirs) parts.push(humanSize(bytes));
    if (dirs) parts.push(plural(dirs, "folder"));
    r.selCount.replaceChildren(document.createTextNode(n + " selected"), parts.length ? h("span", { class: "muted", text: " · " + parts.join(" · ") }) : "");
    const hasDir = dirs > 0 || sel.some((it) => it.is_symlink);
    r.actDownload.disabled = hasDir;
    r.actDownload.title = hasDir ? "Folders can't be downloaded directly — compress them first" : "Download " + plural(n, "file");
    r.actRename.disabled = n !== 1;
    r.actCopy.disabled = false;
    r.actMove.disabled = false;
  }
  const items = vis(p).items;
  const total = p.items.length;
  let text;
  if (p.loading && !total) text = "Loading…";
  else if (p.error) text = "Unavailable";
  else if (p.filter.trim()) text = "Showing " + items.length.toLocaleString() + " of " + plural(total, "item").replace(String(total), total.toLocaleString());
  else {
    const dirs = p.items.filter((it) => it.is_dir).length;
    const bytes = p.items.reduce((a, it) => a + (it.is_dir ? 0 : it.size || 0), 0);
    text = plural(total, "item").replace(String(total), total.toLocaleString()) + (total ? " · " + plural(dirs, "folder") + " · " + humanSize(bytes) : "");
  }
  r.status.textContent = text;
  r.statusExtra.textContent = p.hidden ? "hidden files shown" : "";
}

// ----------------------------------------------------------------- keyboard (grid)

function onGridKey(p, e) {
  if (e.defaultPrevented) return;
  const { items } = vis(p);
  const n = items.length;
  const lay = p._layout || gridLayout(p);
  const step = lay.icons ? lay.cols : 1;
  const page = Math.max(1, Math.floor(p.el.grid.clientHeight / lay.rowH) - 1) * step;
  const mod = modKey(e);
  const move = (to) => {
    e.preventDefault();
    if (!n) return;
    to = Math.max(0, Math.min(n - 1, to));
    if (e.shiftKey) selectRange(p, to, mod);
    else if (mod) setCursor(p, to);
    else selectOnly(p, to);
  };
  const cur = p.cursor;
  switch (e.key) {
    case "ArrowDown": if (e.altKey) break; return move(cur < 0 ? 0 : cur + step);
    case "ArrowUp":
      if (e.altKey) { e.preventDefault(); goUp(p); return; }
      return move(cur < 0 ? n - 1 : cur - step);
    case "ArrowRight":
      if (e.altKey) { e.preventDefault(); goForward(p); return; }
      if (lay.icons) return move(cur + 1);
      break;
    case "ArrowLeft":
      if (e.altKey) { e.preventDefault(); goBack(p); return; }
      if (lay.icons) return move(cur - 1);
      break;
    case "Home": return move(0);
    case "End": return move(n - 1);
    case "PageDown": return move(cur + page);
    case "PageUp": return move(cur - page);
    case " ":
    case "Spacebar":
      e.preventDefault();
      if (cur < 0 && n) { selectOnly(p, 0); return; }
      if (e.shiftKey) selectRange(p, cur, true); else toggleOne(p, cur);
      return;
    case "Enter": {
      e.preventDefault();
      const it = items[cur];
      if (!it) return;
      if (mod && !it.is_dir) downloadItems(p, [it]);
      else if (e.shiftKey && isTextFile(it.name)) openEditor(p, it);
      else openItem(p, it);
      return;
    }
    case "Backspace":
      e.preventDefault();
      if (IS_MAC && e.metaKey && p.sel.size) actDelete(p); else goUp(p);
      return;
    case "Delete":
      e.preventDefault();
      if (p.sel.size) actDelete(p);
      else if (items[cur]) { selectOnly(p, cur); actDelete(p); }
      return;
    case "F2":
      e.preventDefault();
      if (!p.sel.size && items[cur]) selectOnly(p, cur);
      actRename(p);
      return;
    case "Escape":
      if (p.sel.size) { e.preventDefault(); e.stopPropagation(); clearSelection(p); }
      return;
    case "ContextMenu": {
      e.preventDefault();
      const it = items[cur];
      const node = it ? $("#p" + p.uid + "-" + cur) : null;
      const rect = (node || p.el.grid).getBoundingClientRect();
      if (it && !p.sel.has(it.name)) selectOnly(p, cur);
      openItemMenu(p, it || null, rect.left + 24, rect.top + (node ? rect.height : 24));
      return;
    }
  }
  if (e.shiftKey && e.key === "F10") {
    e.preventDefault();
    const rect = p.el.grid.getBoundingClientRect();
    openItemMenu(p, items[cur] || null, rect.left + 24, rect.top + 24);
    return;
  }
  if (mod && !e.altKey) {
    const k = e.key.toLowerCase();
    if (k === "a") { e.preventDefault(); selectAll(p); return; }
    if (k === "c") { e.preventDefault(); clipboardSet(p, "copy"); return; }
    if (k === "x") { e.preventDefault(); clipboardSet(p, "cut"); return; }
    if (k === "v") { e.preventDefault(); pasteInto(p); return; }
    return;
  }
  if (e.altKey || e.ctrlKey || e.metaKey) return;
  switch (e.key) {
    case "v": case "V": e.preventDefault(); setView(p, p.view === "list" ? "icons" : "list"); return;
    case ".": e.preventDefault(); toggleHidden(p); return;
    case "p": case "P": e.preventDefault(); togglePreview(p); return;
    case "e": case "E": {
      const it = items[cur];
      if (it && !it.is_dir && isTextFile(it.name)) { e.preventDefault(); openEditor(p, it); }
      return;
    }
    case "n": case "N": if (e.shiftKey) { e.preventDefault(); promptMkdir(p); } return;
    case "g": case "G": e.preventDefault(); startPathEdit(p); return;
    case "r": case "R": e.preventDefault(); loadListing(p, { keepScroll: true }); return;
  }
}

// ----------------------------------------------------------------- pointer (grid)

let lastPointerType = "mouse";
let suppressClick = false;
const LONG_PRESS_MS = 480;

function hitFromEvent(p, e) {
  const el = e.target.closest && e.target.closest("[data-idx]");
  if (!el || !p.el.grid.contains(el)) return null;
  const idx = +el.dataset.idx;
  const item = vis(p).items[idx];
  return item ? { idx, item, el } : null;
}

function onGridPointerDown(p, e) {
  lastPointerType = e.pointerType || "mouse";
  setActivePane(p);
  const hit = hitFromEvent(p, e);
  if (!hit) return;
  if (e.pointerType === "mouse") {
    if (e.button === 0 && !e.target.closest(".check")) prepareDrag(p, hit, e);
    return;
  }
  // Touch / pen: long-press opens the context menu (shows the full name too).
  const sx = e.clientX, sy = e.clientY;
  let timer = setTimeout(() => {
    timer = 0;
    suppressClick = true;
    if (!p.sel.has(hit.item.name)) { p.sel.add(hit.item.name); p.anchor = hit.item.name; }
    setCursor(p, hit.idx, { scroll: false });
    selectionChanged(p);
    if (navigator.vibrate) navigator.vibrate(8);
    openItemMenu(p, hit.item, sx, sy);
  }, LONG_PRESS_MS);
  const cancel = () => { if (timer) clearTimeout(timer); timer = 0; cleanup(); };
  const onMove = (ev) => { if (Math.hypot(ev.clientX - sx, ev.clientY - sy) > 10) cancel(); };
  const cleanup = () => {
    window.removeEventListener("pointermove", onMove);
    window.removeEventListener("pointerup", cancel);
    window.removeEventListener("pointercancel", cancel);
  };
  window.addEventListener("pointermove", onMove, { passive: true });
  window.addEventListener("pointerup", cancel);
  window.addEventListener("pointercancel", cancel);
}

function onGridClick(p, e) {
  if (suppressClick) { suppressClick = false; e.preventDefault(); return; }
  const hit = hitFromEvent(p, e);
  if (!hit) {
    if (!e.shiftKey && !modKey(e)) clearSelection(p);
    p.el.grid.focus({ preventScroll: true });
    return;
  }
  const touch = lastPointerType === "touch" || lastPointerType === "pen";
  if (e.target.closest(".check") || (touch && p.sel.size > 0)) toggleOne(p, hit.idx);
  else if (touch) { setCursor(p, hit.idx, { scroll: false }); openItem(p, hit.item); }
  else if (e.shiftKey) selectRange(p, hit.idx, modKey(e));
  else if (modKey(e)) toggleOne(p, hit.idx);
  else selectOnly(p, hit.idx);
  if (!touch) p.el.grid.focus({ preventScroll: true });
}

function onGridDblClick(p, e) {
  if (lastPointerType !== "mouse") return;
  const hit = hitFromEvent(p, e);
  if (hit && !e.target.closest(".check")) openItem(p, hit.item);
}

function onGridContext(p, e) {
  e.preventDefault();
  if (suppressClick && lastPointerType !== "mouse") return; // the long-press already opened it
  const hit = hitFromEvent(p, e);
  if (hit) {
    if (!p.sel.has(hit.item.name)) selectOnly(p, hit.idx);
    else setCursor(p, hit.idx, { scroll: false });
  } else clearSelection(p);
  openItemMenu(p, hit ? hit.item : null, e.clientX, e.clientY);
}

// ----------------------------------------------------------------- drag between panes

const drag = { active: false, started: false, src: null, items: [], startX: 0, startY: 0, target: null };

function prepareDrag(p, hit, e) {
  drag.active = true;
  drag.started = false;
  drag.src = p;
  drag.items = p.sel.has(hit.item.name) ? selectedItems(p) : [hit.item];
  drag.startX = e.clientX;
  drag.startY = e.clientY;
  drag.target = null;
  window.addEventListener("pointermove", onDragMove);
  window.addEventListener("pointerup", onDragUp);
}

function onDragMove(e) {
  if (!drag.active) return;
  if (!drag.started) {
    if (Math.hypot(e.clientX - drag.startX, e.clientY - drag.startY) < 6) return;
    drag.started = true;
    startGhost();
  }
  $("#ghost").style.transform = "translate(" + (e.clientX + 14) + "px, " + (e.clientY + 12) + "px)";
  updateDropTarget(e.clientX, e.clientY);
}

function startGhost() {
  const g = $("#ghost");
  const first = drag.items[0];
  const gi = $(".ghost-icon", g);
  setIcon(gi, iconNameFor(first));
  gi.classList.toggle("dir", !!first.is_dir);
  $(".ghost-name", g).textContent = drag.items.length === 1 ? first.name : plural(drag.items.length, "item");
  const badge = $(".ghost-badge", g);
  badge.textContent = String(drag.items.length);
  badge.classList.toggle("show", drag.items.length > 1);
  g.classList.remove("settling");
  g.classList.add("visible");
  document.body.style.userSelect = "none";
  markDragging(true);
}

function markDragging(on) {
  if (!drag.src || !drag.src.el) return;
  const names = new Set(drag.items.map((it) => it.name));
  for (const el of drag.src.el.pool.values()) {
    const nodes = el.classList.contains("vrow") ? Array.from(el.children) : [el];
    for (const node of nodes) {
      const it = vis(drag.src).items[+node.dataset.idx];
      node.classList.toggle("dragging", on && !!it && names.has(it.name));
    }
  }
}

function clearDropHighlights() {
  $$(".pane.drop-target").forEach((el) => el.classList.remove("drop-target"));
  $$(".drop-into").forEach((el) => el.classList.remove("drop-into"));
}

function paneFromEl(el) {
  const root = el && el.closest(".pane");
  return root ? state.panes.find((q) => q.el && q.el.root === root) || null : null;
}

function updateDropTarget(x, y) {
  clearDropHighlights();
  drag.target = null;
  const under = document.elementFromPoint(x, y);
  if (!under) return;
  const tp = paneFromEl(under);
  if (!tp) return;
  const crumb = under.closest(".crumb");
  if (crumb && crumb.dataset.path && tp === drag.src && crumb.dataset.path !== tp.path) {
    crumb.classList.add("drop-into");
    drag.target = { kind: "dir", pane: tp, dir: crumb.dataset.path, el: crumb };
    return;
  }
  const node = under.closest("[data-idx]");
  if (node && tp.el.grid.contains(node)) {
    const it = vis(tp).items[+node.dataset.idx];
    const self = tp === drag.src && drag.items.some((d) => d.name === (it && it.name));
    if (it && it.is_dir && !self) {
      node.classList.add("drop-into");
      drag.target = { kind: "dir", pane: tp, dir: joinPath(tp.path, it.name, tp.pathStyle), el: node };
      return;
    }
  }
  if (tp !== drag.src) {
    tp.el.root.classList.add("drop-target");
    drag.target = { kind: "pane", pane: tp, dir: tp.path, el: tp.el.root };
  }
}

function onDragUp(e) {
  window.removeEventListener("pointermove", onDragMove);
  window.removeEventListener("pointerup", onDragUp);
  if (!drag.active) return;
  const started = drag.started;
  const target = drag.target;
  const items = drag.items;
  const src = drag.src;
  markDragging(false);
  document.body.style.userSelect = "";
  drag.active = false;
  drag.started = false;
  const g = $("#ghost");
  clearDropHighlights();
  if (!started) { g.classList.remove("visible"); return; }
  suppressClick = true;
  setTimeout(() => { suppressClick = false; }, 0);
  if (!target) { g.classList.remove("visible"); return; }
  const rect = target.el.getBoundingClientRect();
  g.classList.add("settling");
  g.style.transform = "translate(" + (rect.left + rect.width / 2 - 20) + "px, " + (rect.top + rect.height / 2 - 14) + "px) scale(.6)";
  setTimeout(() => g.classList.remove("visible", "settling"), 200);

  if (target.pane === src) {
    // Same pane: a move into a subfolder or an ancestor (instant rename).
    moveWithinSource(src, items, target.dir);
    return;
  }
  const what = items.length === 1 ? items[0].name : plural(items.length, "item");
  openMenu([
    { title: what + " → " + locationLabel(target.pane.server, target.dir) },
    { label: "Copy here", icon: "copy", run: () => runTransfer(src, items, target.pane, target.dir, "copy") },
    { label: "Move here", icon: "move", run: () => runTransfer(src, items, target.pane, target.dir, "move") },
    "sep",
    { label: "Cancel", icon: "x", run: () => {} },
  ], { x: e.clientX, y: e.clientY, label: "Drop action" });
}

// ----------------------------------------------------------------- item actions

function fullPath(p, item) { return joinPath(p.path, item.name, p.pathStyle); }

function openItem(p, item) {
  if (!item) return;
  if (item.is_dir) { navigate(p, fullPath(p, item)); return; }
  openPreview(p, item);
}

function itemMenuItems(p, item, sel) {
  const multi = sel.length > 1;
  const oneFile = item && !item.is_dir && !multi;
  const anyDir = sel.some((it) => it.is_dir || it.is_symlink);
  const targets = state.panes.filter((q) => q !== p);
  const list = [];
  if (item) {
    list.push({ title: multi ? plural(sel.length, "item") + " selected" : item.name });
    if (item.is_dir) list.push({ label: "Open", icon: "arrow-right", hint: "Enter", run: () => navigate(p, fullPath(p, item)) });
    else list.push({ label: "Preview", icon: "eye", hint: "Enter", disabled: multi, run: () => openPreview(p, item) });
    if (oneFile && isTextFile(item.name)) list.push({ label: "Edit", icon: "edit", hint: "E", run: () => openEditor(p, item) });
    if (!anyDir) list.push({ label: multi ? "Download " + sel.length + " files" : "Download", icon: "download", hint: MOD + "+Enter", run: () => downloadItems(p, sel) });
    list.push("sep");
    list.push({ label: "Copy to…", icon: "copy", run: () => transferDialog(p, "copy") });
    list.push({ label: "Move to…", icon: "move", run: () => transferDialog(p, "move") });
    for (const q of targets.slice(0, 3)) {
      list.push({ label: "Copy to pane " + paneNumber(q) + " · " + sourceName(q.server), icon: "arrow-right", run: () => runTransfer(p, sel, q, q.path, "copy") });
    }
    list.push({ label: "Copy", icon: "copy", hint: MOD + "+C", run: () => clipboardSet(p, "copy") });
    list.push({ label: "Cut", icon: "scissors", hint: MOD + "+X", run: () => clipboardSet(p, "cut") });
    list.push("sep");
    list.push({ label: "Rename…", icon: "edit", hint: "F2", disabled: multi, run: () => actRename(p) });
    list.push({ label: "Duplicate", icon: "duplicate", disabled: multi, run: () => actDuplicate(p, item) });
    list.push({ label: "Compress…", icon: "archive", run: () => actCompress(p) });
    if (oneFile && isArchiveFile(item.name)) list.push({ label: "Extract here", icon: "extract", run: () => actExtract(p, item) });
    list.push({ label: "Permissions…", icon: "lock", disabled: multi, run: () => actChmod(p, item) });
    if (oneFile) list.push({ label: "SHA-256 checksum", icon: "hash", run: () => actChecksum(p, item) });
    list.push({ label: "Copy path", icon: "copy", disabled: multi, run: () => copyPath(p, item) });
    list.push({ label: "Properties", icon: "info", disabled: multi, run: () => openPreview(p, item) });
    list.push("sep");
    list.push({ label: multi ? "Delete " + sel.length + " items…" : "Delete…", icon: "trash", danger: true, hint: "Del", run: () => actDelete(p) });
  } else {
    list.push({ title: locationLabel(p.server, p.path) });
    list.push({ label: "New folder", icon: "folder-plus", hint: "Shift+N", run: () => promptMkdir(p) });
    list.push({ label: "New file", icon: "file-plus", run: () => promptNewFile(p) });
    list.push({ label: "Upload files…", icon: "upload", run: () => p.el.fileInput.click() });
    list.push({ label: "Paste", icon: "clipboard", hint: MOD + "+V", disabled: !state.clipboard, run: () => pasteInto(p) });
    list.push("sep");
    list.push({ label: "Select all", icon: "check", hint: MOD + "+A", run: () => selectAll(p) });
    list.push({ label: "Refresh", icon: "refresh", hint: "R", run: () => loadListing(p, { keepScroll: true }) });
  }
  return list;
}

function openItemMenu(p, item, x, y) {
  setActivePane(p);
  const sel = item ? (p.sel.has(item.name) ? selectedItems(p) : [item]) : [];
  openMenu(itemMenuItems(p, item, sel), { x, y, label: item ? "Actions for " + item.name : "Folder actions" });
}

// ----------------------------------------------------------------- downloads

function downloadSelection(p) {
  const files = selectedItems(p).filter((it) => !it.is_dir && !it.is_symlink);
  if (!files.length) { toast("Select one or more files to download", "error"); return; }
  downloadItems(p, files);
}

function downloadItems(p, items) {
  const files = items.filter((it) => !it.is_dir && !it.is_symlink);
  if (files.length !== items.length) toast("Folders and links can't be downloaded directly — compress folders first, then download the archive.", "error");
  for (const f of files) {
    const t = newTransfer({ kind: "download", label: f.name, srcServer: p.server, srcPath: fullPath(p, f), total: f.size || 0 });
    t.retry = () => downloadItems(p, [f]);
    enqueue(t, startDownload);
  }
  if (files.length > 1) toast("Downloading " + plural(files.length, "file") + " — your browser may ask to allow multiple downloads.", "", { action: { label: "View", run: openTransfers } });
}

// ----------------------------------------------------------------- create / rename / delete

function nameValidator(p, existing = null) {
  return (name) => {
    const err = componentNameError(name, p.pathStyle);
    if (err) return err;
    if (existing !== null) {
      const key = p.caseInsensitive ? name.toLowerCase() : name;
      for (const it of p.items) {
        const k = p.caseInsensitive ? it.name.toLowerCase() : it.name;
        if (k === key && it.name !== existing) return "“" + it.name + "” already exists here";
      }
    }
    return "";
  };
}

async function promptMkdir(p) {
  const name = await promptDialog({
    title: "New folder",
    message: h("span", null, "Create a folder in ", h("strong", { text: locationLabel(p.server, p.path) })),
    value: "untitled folder", okLabel: "Create folder", selectStem: false,
    validate: nameValidator(p, ""),
  });
  if (name === null) return;
  try {
    await postJSON("/api/mkdir", { server: p.server, dir: p.path, name });
  } catch (e) {
    toast("Couldn't create “" + name + "”: " + friendlyError(e, p.server), "error");
    return;
  }
  const created = joinPath(p.path, name, p.pathStyle);
  await loadListing(p, { keepScroll: true, focusName: name, quiet: true });
  const i = vis(p).index.get(name);
  if (i !== undefined) selectOnly(p, i);
  toast("Created folder “" + name + "”", "success", {
    action: { label: "Undo", run: () => undoRemove(p.server, created, false, "folder “" + name + "”") },
  });
}

async function promptNewFile(p) {
  const name = await promptDialog({
    title: "New file",
    message: h("span", null, "Create an empty file in ", h("strong", { text: locationLabel(p.server, p.path) })),
    value: "untitled.txt", okLabel: "Create file",
    validate: nameValidator(p, ""),
  });
  if (name === null) return;
  try {
    await postJSON("/api/touch", { server: p.server, dir: p.path, name });
  } catch (e) {
    toast("Couldn't create “" + name + "”: " + friendlyError(e, p.server), "error");
    return;
  }
  await loadListing(p, { keepScroll: true, focusName: name, quiet: true });
  const i = vis(p).index.get(name);
  if (i !== undefined) selectOnly(p, i);
  toast("Created “" + name + "”", "success");
  if (isTextFile(name)) openEditor(p, { name, is_dir: false, size: 0 });
}

async function undoRemove(server, path, recursive, label) {
  try {
    await postJSON("/api/rm", { server, path, recursive: recursive ? "true" : "false" });
    toast("Removed " + label, "success");
  } catch (e) {
    toast("Undo failed: " + friendlyError(e, server), "error");
  }
  refreshPanesAt(server);
}

async function actRename(p) {
  const sel = selectedItems(p);
  if (sel.length !== 1) return;
  const item = sel[0];
  const name = await promptDialog({
    title: "Rename",
    message: h("span", null, "Rename ", h("strong", { text: "“" + item.name + "”" }), " in " + locationLabel(p.server, p.path)),
    value: item.name, okLabel: "Rename",
    validate: nameValidator(p, item.name),
  });
  if (name === null || name === item.name) return;
  if (!(await renameItem(p, item.name, name))) return;
  toast("Renamed to “" + name + "”", "success", {
    action: { label: "Undo", run: async () => { if (await renameItem(p, name, item.name)) toast("Rename undone", "success"); } },
  });
}

async function renameItem(p, from, to) {
  try {
    await postJSON("/api/mv", { server: p.server, from: joinPath(p.path, from, p.pathStyle), name: to });
  } catch (e) {
    toast("Rename failed: " + friendlyError(e, p.server), "error");
    return false;
  }
  p.sel.clear();
  await loadListing(p, { keepScroll: true, focusName: to, quiet: true });
  const i = vis(p).index.get(to);
  if (i !== undefined) selectOnly(p, i);
  refreshPanesAt(p.server, p.path, p.pathStyle);
  return true;
}

async function actDelete(p) {
  const sel = selectedItems(p);
  if (!sel.length) return;
  const dirs = sel.filter((it) => it.is_dir).length;
  const bytes = sel.reduce((a, it) => a + (it.is_dir ? 0 : it.size || 0), 0);
  const what = sel.length === 1 ? "“" + sel[0].name + "”" : plural(sel.length, "item");
  const notes = {};
  for (const it of sel) if (it.is_dir) notes[it.name] = { text: "folder + contents", warn: true };
  const ok = await confirmDialog({
    title: "Delete " + (sel.length === 1 ? (sel[0].is_dir ? "folder" : "file") : sel.length + " items") + "?",
    message: h("span", null, "From ", h("strong", { text: locationLabel(p.server, p.path) }), ":"),
    items: sel, notes,
    extra: [callout("danger", "alert",
      "This permanently deletes " + what +
      (dirs ? " — including everything inside " + plural(dirs, "folder") : "") +
      (bytes ? " (" + humanSize(bytes) + " of files)" : "") +
      ". There is no trash and it cannot be undone.")],
    okLabel: sel.length === 1 ? "Delete" : "Delete " + sel.length + " items",
    danger: true,
  });
  if (!ok) return;
  let done = 0;
  const failures = [];
  for (const item of sel) {
    try {
      await postJSON("/api/rm", { server: p.server, path: fullPath(p, item), recursive: item.is_dir ? "true" : "false" });
      done++;
    } catch (e) {
      failures.push(item.name + ": " + friendlyError(e, p.server));
    }
  }
  p.sel.clear();
  await loadListing(p, { keepScroll: true, quiet: true });
  refreshPanesAt(p.server, p.path, p.pathStyle);
  if (done) toast("Deleted " + plural(done, "item"), "success");
  if (failures.length) toast("Couldn't delete " + failures.join("; "), "error");
}

async function actDuplicate(p, item) {
  const err = componentNameError(item.name, p.pathStyle);
  if (err) { toast("Can't duplicate: " + err, "error"); return; }
  let res;
  try {
    res = await postJSON("/api/duplicate", { server: p.server, path: fullPath(p, item) });
  } catch (e) {
    toast("Duplicate failed: " + friendlyError(e, p.server), "error");
    return;
  }
  const copyName = res.path ? baseName(res.path, p.pathStyle) : "";
  await loadListing(p, { keepScroll: true, focusName: copyName, quiet: true });
  const i = vis(p).index.get(copyName);
  if (i !== undefined) selectOnly(p, i);
  toast("Duplicated as “" + copyName + "”", "success", res.path ? {
    action: { label: "Undo", run: () => undoRemove(p.server, res.path, !!item.is_dir, "“" + copyName + "”") },
  } : {});
}

// ----------------------------------------------------------------- archives / permissions / checksum

let archiveFormats = null;
async function loadArchiveFormats() {
  if (archiveFormats) return archiveFormats;
  try { archiveFormats = await getJSON("/api/formats"); } catch { archiveFormats = ["zip", "tar.gz", "tar.bz2", "tar.xz", "tar"]; }
  return archiveFormats;
}

async function actCompress(p) {
  const items = selectedItems(p);
  if (!items.length) { toast("Select items to compress", "error"); return; }
  for (const item of items) {
    const error = componentNameError(item.name, p.pathStyle);
    if (error) { toast("Can't archive “" + item.name + "”: " + error, "error"); return; }
  }
  const formats = await loadArchiveFormats();
  const base = defaultArchiveBase(items);
  const fmtId = "dlg-fmt-" + ++dialogSeq, nameId = "dlg-name-" + dialogSeq, errId = nameId + "-err";
  const select = h("select", { id: fmtId, class: "select-input" }, formats.map((f) => h("option", { value: f, text: f })));
  const input = h("input", { id: nameId, class: "text-input", type: "text", spellcheck: "false", "aria-describedby": errId });
  const err = h("div", { id: errId, class: "field-error", "aria-live": "polite" });
  let edited = false;
  const apply = () => { if (!edited) input.value = base + "." + select.value; };
  apply();
  select.addEventListener("change", apply);
  const validate = () => {
    let msg = componentNameError(input.value, p.pathStyle);
    if (!msg && p.byName.has(input.value)) msg = "“" + input.value + "” already exists here and would be replaced — choose another name";
    err.textContent = msg;
    input.setAttribute("aria-invalid", msg ? "true" : "false");
    return !msg;
  };
  input.addEventListener("input", () => { edited = true; validate(); });
  const result = await dialog({
    title: "Compress " + (items.length === 1 ? "“" + items[0].name + "”" : plural(items.length, "item")),
    body: [
      h("p", { class: "modal-desc" }, "Creates a new archive in ", h("strong", { text: locationLabel(p.server, p.path) }), ". The originals are left untouched."),
      itemList(items),
      h("div", { class: "modal-field" }, h("label", { for: fmtId, text: "Format" }), select),
      h("div", { class: "modal-field" }, h("label", { for: nameId, text: "Archive name" }), input, err),
    ],
    actions: [
      { label: "Cancel", value: null },
      { label: "Create archive", kind: "primary", validate, value: () => ({ archive: input.value, format: select.value }) },
    ],
    initialFocus: input,
    onKey: (e) => {
      if (e.key === "Enter" && e.target === input) { e.preventDefault(); if (validate()) closeModal({ archive: input.value, format: select.value }); return true; }
      return false;
    },
  });
  if (!result) return;
  const params = new URLSearchParams();
  params.set("server", p.server);
  params.set("dir", p.path);
  params.set("archive", result.archive);
  params.set("format", result.format);
  for (const it of items) params.append("name", it.name);
  const dismiss = toast("Compressing " + plural(items.length, "item") + "…");
  try {
    await postForm("/api/compress", params);
  } catch (e) {
    dismiss();
    toast("Compress failed: " + friendlyError(e, p.server), "error");
    return;
  }
  dismiss();
  await loadListing(p, { keepScroll: true, focusName: result.archive, quiet: true });
  const i = vis(p).index.get(result.archive);
  if (i !== undefined) selectOnly(p, i);
  toast("Created “" + result.archive + "”", "success", {
    action: { label: "Download", run: () => { const it = p.byName.get(result.archive); if (it) downloadItems(p, [it]); } },
  });
}

async function actExtract(p, item) {
  const ok = await confirmDialog({
    title: "Extract “" + item.name + "”?",
    message: h("span", null, "Unpacks the archive into ", h("strong", { text: locationLabel(p.server, p.path) }), "."),
    extra: [callout("warn", "alert", "Files in the archive replace existing files with the same names in this folder.")],
    okLabel: "Extract here",
  });
  if (!ok) return;
  const dismiss = toast("Extracting “" + item.name + "”…");
  try {
    await postJSON("/api/extract", { server: p.server, path: fullPath(p, item) });
  } catch (e) {
    dismiss();
    toast("Extract failed: " + friendlyError(e, p.server), "error");
    return;
  }
  dismiss();
  toast("Extracted “" + item.name + "”", "success");
  loadListing(p, { keepScroll: true, quiet: true });
}

async function actChmod(p, item) {
  const cur = item.mode != null ? (item.mode & 0o777).toString(8).padStart(3, "0") : "644";
  const mode = await promptDialog({
    title: "Permissions",
    message: h("span", null, "Octal mode for ", h("strong", { text: "“" + item.name + "”" }), " (currently " + modeString(item.mode, item.is_dir).trim() + ")"),
    value: cur, okLabel: "Apply", selectStem: false, mono: true,
    hint: "e.g. 644 = rw-r--r--, 755 = rwxr-xr-x, 600 = rw-------",
    validate: (v) => (/^[0-7]{3,4}$/.test(v.trim()) ? "" : "Use 3 or 4 octal digits, e.g. 644"),
  });
  if (mode === null) return;
  try {
    await postJSON("/api/chmod", { server: p.server, path: fullPath(p, item), mode: mode.trim() });
  } catch (e) {
    toast("Permissions failed: " + friendlyError(e, p.server), "error");
    return;
  }
  toast("Set " + item.name + " to " + mode.trim(), "success", { action: cur !== mode.trim() ? { label: "Undo", run: async () => {
    try { await postJSON("/api/chmod", { server: p.server, path: fullPath(p, item), mode: cur }); toast("Restored " + cur, "success"); loadListing(p, { keepScroll: true, quiet: true }); }
    catch (e) { toast("Undo failed: " + friendlyError(e, p.server), "error"); }
  } } : null });
  loadListing(p, { keepScroll: true, quiet: true });
}

async function actChecksum(p, item) {
  const box = h("div", { class: "checksum-box", text: "Computing…", "aria-live": "polite" });
  const copyBtn = h("button", { class: "btn", type: "button", text: "Copy", disabled: true });
  let hash = "";
  copyBtn.addEventListener("click", async () => {
    const ok = await copyText(hash);
    copyBtn.textContent = ok ? "Copied" : "Copy";
    if (ok) setTimeout(() => { copyBtn.textContent = "Copy"; }, 1400);
  });
  const id = "dlg-" + ++dialogSeq;
  showModal([
    h("h2", { id, text: "SHA-256" }),
    h("p", { class: "modal-desc", text: locationLabel(p.server, fullPath(p, item)) }),
    box,
    h("div", { class: "modal-actions" }, copyBtn, h("button", { class: "btn primary", type: "button", text: "Close", onclick: () => closeModal(null) })),
  ], { labelledBy: id });
  try {
    const out = await getJSON("/api/checksum", { server: p.server, path: fullPath(p, item) });
    hash = out.sha256 || "";
    box.textContent = hash;
    copyBtn.disabled = !hash;
  } catch (e) {
    box.textContent = "Checksum failed: " + friendlyError(e, p.server);
  }
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
  const ta = h("textarea", { class: "sr-only", value: text, "aria-hidden": "true" });
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { ok = false; }
  ta.remove();
  return ok;
}

async function copyPath(p, item) {
  const full = fullPath(p, item);
  const ok = await copyText(full);
  toast(ok ? "Copied path" : "Path: " + full, ok ? "success" : "");
}

// ----------------------------------------------------------------- copy / move

// transferDialog asks where to copy/move the selection, states exactly what
// will happen (including replacements), then runs it.
async function transferDialog(p, kind) {
  const items = selectedItems(p);
  if (!items.length) return;
  const verb = kind === "move" ? "Move" : "Copy";
  const def = other(p);
  const srcId = "dlg-dst-" + ++dialogSeq, pathId = srcId + "-path", errId = srcId + "-err";
  const sourceSel = h("select", { id: srcId, class: "select-input", "aria-label": "Destination source" });
  const addOpt = (value, label, disabled) => sourceSel.append(h("option", { value, text: label, disabled: !!disabled }));
  state.panes.forEach((q, i) => { if (q !== p) addOpt("pane:" + q.uid, "Pane " + (i + 1) + " · " + sourceName(q.server)); });
  addOpt("src:", "Local");
  for (const s of state.servers) addOpt("src:" + s.name, s.name + (s.reachable ? "" : " — offline"), !s.reachable);
  sourceSel.value = def ? "pane:" + def.uid : "src:" + p.server;
  const pathIn = h("input", { id: pathId, class: "text-input mono", type: "text", spellcheck: "false", autocomplete: "off", "aria-describedby": errId, "aria-label": "Destination folder" });
  const err = h("div", { id: errId, class: "field-error", "aria-live": "polite" });
  const conflictsBox = h("div");
  const resolve = () => {
    const v = sourceSel.value;
    if (v.startsWith("pane:")) {
      const q = state.panes.find((x) => "pane:" + x.uid === v);
      return q ? { server: q.server, pathStyle: q.pathStyle, caseInsensitive: q.caseInsensitive, pane: q, browseRoot: q.browseRoot } : null;
    }
    const server = v.slice(4);
    const src = sourceInfo(server) || state.local;
    const style = normalizePathStyle(src.path_style);
    return { server, pathStyle: style, caseInsensitive: !!(src.case_insensitive || style === "windows"), pane: null, browseRoot: src.browse_root || (style === "windows" ? "C:\\" : "/") };
  };
  const syncPath = () => {
    const d = resolve();
    if (!d) return;
    if (d.pane) pathIn.value = d.pane.path;
    else {
      const src = sourceInfo(d.server) || state.local;
      pathIn.value = (store.get("lastPath", {}) || {})[d.server] || src.initial_root || d.browseRoot;
    }
    check();
  };
  const check = () => {
    const d = resolve();
    let msg = "";
    const dir = cleanPath(pathIn.value.trim(), d ? d.pathStyle : "posix");
    if (!d) msg = "Choose a destination";
    else if (!pathIn.value.trim()) msg = "Enter a destination folder";
    else if (!pathWithin(d.browseRoot, dir, d.pathStyle)) msg = "Outside " + sourceName(d.server) + "'s browsable root (" + d.browseRoot + ")";
    else if (d.server === p.server && dir === p.path) msg = "That's the folder the items are already in";
    else {
      const nsErr = batchNamespaceError(items, d.pathStyle, d.caseInsensitive);
      if (nsErr) msg = nsErr;
    }
    err.textContent = msg;
    pathIn.setAttribute("aria-invalid", msg ? "true" : "false");
    // Known replacements: only when a pane currently shows the destination.
    conflictsBox.replaceChildren();
    if (!msg && d) {
      const view = state.panes.find((q) => q.server === d.server && q.path === dir && q.items.length);
      if (view) {
        const clash = items.filter((it) => view.byName.has(it.name));
        if (clash.length) {
          conflictsBox.append(callout("warn", "alert",
            plural(clash.length, "item") + " already exist" + (clash.length === 1 ? "s" : "") + " there and will be replaced: " +
            clash.slice(0, 5).map((it) => "“" + it.name + "”").join(", ") + (clash.length > 5 ? "…" : "")));
        }
      }
    }
    return !msg;
  };
  sourceSel.addEventListener("change", syncPath);
  pathIn.addEventListener("input", debounce(check, 120));
  syncPath();
  const dirs = items.filter((it) => it.is_dir).length;
  const bytes = items.reduce((a, it) => a + (it.is_dir ? 0 : it.size || 0), 0);
  const summary = plural(items.length, "item") + (bytes ? " · " + humanSize(bytes) : "") + (dirs ? " · " + plural(dirs, "folder") + " with all contents" : "");
  const res = await dialog({
    title: verb + " " + (items.length === 1 ? "“" + items[0].name + "”" : plural(items.length, "item")),
    className: "wide",
    body: [
      h("p", { class: "modal-desc" }, verb + " from ", h("strong", { text: locationLabel(p.server, p.path) }), " — " + summary),
      itemList(items),
      h("div", { class: "modal-field" }, h("label", { for: pathId, text: "Destination" }), h("div", { class: "dest-row" }, sourceSel, pathIn), err),
      conflictsBox,
      kind === "move"
        ? callout("", "info", "Each item is copied first and removed from " + sourceName(p.server) + " only after its copy succeeds.")
        : callout("", "info", "The originals stay where they are. Transfers are checksummed and keep running while you browse."),
    ],
    actions: [
      { label: "Cancel", value: null },
      { label: verb + " " + (items.length === 1 ? "" : items.length + " items"), kind: "primary", validate: check, value: () => ({ d: resolve(), dir: cleanPath(pathIn.value.trim(), resolve().pathStyle) }) },
    ],
    onKey: (e) => {
      if (e.key === "Enter" && e.target === pathIn) {
        e.preventDefault();
        if (check()) closeModal({ d: resolve(), dir: cleanPath(pathIn.value.trim(), resolve().pathStyle) });
        return true;
      }
      return false;
    },
  });
  if (!res) return;
  runTransfer(p, items, res.d, res.dir, kind);
}

// runTransfer: copy/move `items` from the source (a pane or clipboard
// descriptor with server/path/pathStyle) into dstDir on the destination
// (a pane or a {server, pathStyle, caseInsensitive} descriptor).
async function runTransfer(sp, items, dp, dstDir, kind) {
  const namespaceError = batchNamespaceError(items, dp.pathStyle, dp.caseInsensitive);
  if (namespaceError) { toast(namespaceError, "error"); return; }
  if (sp.server === dp.server && cleanPath(sp.path, sp.pathStyle) === cleanPath(dstDir, dp.pathStyle)) {
    toast("Source and destination are the same folder", "error");
    return;
  }
  // A move within one source is an instant rename, no data transfer needed.
  if (kind === "move" && sp.server === dp.server) {
    await moveWithinSource(sp, items, dstDir);
    return;
  }
  for (const item of items) {
    const srcPath = joinPath(sp.path, item.name, sp.pathStyle);
    const dstPath = joinPath(dstDir, item.name, dp.pathStyle);
    const t = newTransfer({
      kind, label: item.name, srcServer: sp.server, srcPath, dstServer: dp.server, dstPath,
      total: item.is_dir ? 0 : item.size || 0,
    });
    const start = async () => {
      t.state = "running";
      t.startedAt = Date.now();
      renderTransfer(t);
      let resp;
      try {
        resp = await postJSON(kind === "move" ? "/api/move" : "/api/copy", {
          srcServer: sp.server, srcPath, dstServer: dp.server, dstPath, recursive: item.is_dir ? "1" : "",
        });
      } catch (e) {
        finishTransfer(t, { error: friendlyError(e, dp.server || sp.server) });
        return;
      }
      bindHub(t, resp.id);
    };
    t.onDone = (ok) => {
      refreshPanesAt(dp.server, dstDir, dp.pathStyle);
      if (kind === "move") refreshPanesAt(sp.server, sp.path, sp.pathStyle);
      if (!ok) return;
    };
    t.retry = () => runTransfer(sp, [item], dp, dstDir, kind);
    start();
  }
  if (sp.sel && kind === "move") { sp.sel.clear(); selectionChanged(sp); }
  toast((kind === "move" ? "Moving " : "Copying ") + plural(items.length, "item") + " to " + locationLabel(dp.server, dstDir), "", { action: { label: "View", run: openTransfers } });
}

// moveWithinSource renames items into another folder on the same source.
async function moveWithinSource(sp, items, dstDir) {
  const moved = [];
  const failures = [];
  for (const item of items) {
    if (joinPath(sp.path, item.name, sp.pathStyle) === cleanPath(dstDir, sp.pathStyle)) continue;
    const from = joinPath(sp.path, item.name, sp.pathStyle);
    const to = joinPath(dstDir, item.name, sp.pathStyle);
    try {
      await postJSON("/api/mv", { server: sp.server, from, to });
      moved.push({ from, to });
    } catch (e) {
      failures.push(item.name + ": " + friendlyError(e, sp.server));
    }
  }
  if (sp.sel) sp.sel.clear();
  refreshPanesAt(sp.server, sp.path, sp.pathStyle);
  refreshPanesAt(sp.server, dstDir, sp.pathStyle);
  if (moved.length) {
    toast("Moved " + plural(moved.length, "item") + " to " + dstDir, "success", {
      action: { label: "Undo", run: async () => {
        let back = 0;
        for (const m of moved) {
          try { await postJSON("/api/mv", { server: sp.server, from: m.to, to: m.from }); back++; } catch (e) { toast("Undo failed: " + friendlyError(e, sp.server), "error"); }
        }
        refreshPanesAt(sp.server, sp.path, sp.pathStyle);
        refreshPanesAt(sp.server, dstDir, sp.pathStyle);
        if (back) toast("Moved " + plural(back, "item") + " back", "success");
      } },
    });
  }
  if (failures.length) toast("Couldn't move " + failures.join("; "), "error");
}

// ----------------------------------------------------------------- clipboard (between panes)

function clipboardSet(p, mode) {
  const items = selectedItems(p);
  if (!items.length) {
    const it = vis(p).items[p.cursor];
    if (!it) { toast("Select something to " + (mode === "cut" ? "cut" : "copy"), "error"); return; }
    items.push(it);
  }
  state.clipboard = {
    mode, server: p.server, dir: p.path, pathStyle: p.pathStyle,
    items: items.map((it) => ({ name: it.name, is_dir: it.is_dir, size: it.size, is_symlink: it.is_symlink })),
    names: new Set(items.map((it) => it.name)),
  };
  for (const q of state.panes) paintSelection(q);
  toast((mode === "cut" ? "Cut " : "Copied ") + plural(items.length, "item") + " — paste with " + MOD + "+V in any pane", "success");
}

async function pasteInto(p) {
  const c = state.clipboard;
  if (!c) { toast("Nothing to paste — copy or cut items first (" + MOD + "+C / " + MOD + "+X)", "error"); return; }
  if (c.server === p.server && cleanPath(c.dir, c.pathStyle) === cleanPath(p.path, p.pathStyle)) {
    toast(c.mode === "cut" ? "The items are already in this folder" : "Source and destination are the same folder — use Duplicate instead", "error");
    return;
  }
  const sp = { server: c.server, path: c.dir, pathStyle: c.pathStyle };
  if (c.mode === "cut") state.clipboard = null;
  await runTransfer(sp, c.items, p, p.path, c.mode === "cut" ? "move" : "copy");
  for (const q of state.panes) paintSelection(q);
}

// ----------------------------------------------------------------- uploads

function setupFileDropZone(p) {
  const el = p.el.root;
  const isFileDrag = (e) => e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files");
  let depth = 0;
  el.addEventListener("dragenter", (e) => {
    if (!isFileDrag(e)) return;
    e.preventDefault();
    depth++;
    setActivePane(p);
    p.el.dropDest.textContent = locationLabel(p.server, p.path);
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
    const files = e.dataTransfer.files && e.dataTransfer.files.length ? Array.from(e.dataTransfer.files) : [];
    if (files.length) uploadFiles(p, files);
  });
}

async function uploadFiles(p, files) {
  const namespaceError = batchNamespaceError(files.map((f) => ({ name: f.name })), p.pathStyle, p.caseInsensitive);
  if (namespaceError) { toast(namespaceError, "error"); return; }
  // State exactly what will be replaced before overwriting anything.
  const clash = files.filter((f) => p.byName.has(f.name));
  let todo = files;
  if (clash.length) {
    const choice = await dialog({
      title: "Replace " + plural(clash.length, "file") + "?",
      body: [
        h("p", { class: "modal-desc" }, "These already exist in ", h("strong", { text: locationLabel(p.server, p.path) }), ":"),
        itemList(clash.map((f) => p.byName.get(f.name))),
        callout("warn", "alert", "Replacing overwrites the existing files. Skipping uploads only the " + plural(files.length - clash.length, "new file") + "."),
      ],
      actions: [
        { label: "Cancel", value: null },
        { label: "Skip existing", value: "skip", disabled: files.length === clash.length },
        { label: "Replace " + plural(clash.length, "file"), value: "replace", kind: "primary" },
      ],
    });
    if (!choice) return;
    if (choice === "skip") todo = files.filter((f) => !p.byName.has(f.name));
  }
  const dest = { server: p.server, dir: p.path, pathStyle: p.pathStyle };
  for (const file of todo) queueUpload(dest, file);
  if (todo.length) toast("Uploading " + plural(todo.length, "file") + " to " + locationLabel(dest.server, dest.dir), "", { action: { label: "View", run: openTransfers } });
}

function queueUpload(dest, file) {
  const t = newTransfer({ kind: "upload", label: file.name, srcPath: file.name, dstServer: dest.server, dstPath: joinPath(dest.dir, file.name, dest.pathStyle), total: file.size });
  t.file = file;
  t.dest = dest;
  t.retry = () => queueUpload(dest, file);
  t.onDone = () => refreshPanesAt(dest.server, dest.dir, dest.pathStyle);
  enqueue(t, startUpload);
}

// startUpload sends one file and reports both legs of its journey: browser →
// controller (XHR progress) and, for a managed server, controller → agent
// (the hub's progress feed). A server upload's bar splits 50/50 between legs.
function startUpload(t) {
  const { dest, file } = t;
  const toServer = !!dest.server;
  t.phase = toServer ? "Sending to controller" : "Writing";
  t.legShare = toServer ? 0.5 : 1;
  const xhr = new XMLHttpRequest();
  t.cancel = () => xhr.abort();
  xhr.open("POST", api("/api/upload", { server: dest.server, dir: dest.dir, name: file.name }).toString());
  xhr.setRequestHeader("X-Fleet-Token", TOKEN);
  let lastT = Date.now(), lastB = 0;
  xhr.upload.onprogress = (e) => {
    if (!e.lengthComputable) return;
    const now = Date.now();
    if (now - lastT > 400) {
      const inst = ((e.loaded - lastB) * 1000) / (now - lastT);
      t.rate = t.rate ? t.rate * 0.6 + inst * 0.4 : inst;
      lastT = now; lastB = e.loaded;
    }
    t.bytes = e.loaded;
    t.total = e.total;
    t.percent = (e.loaded / e.total) * 100 * t.legShare;
    renderTransfer(t);
  };
  xhr.onerror = () => finishTransfer(t, { error: "Network error while sending to the controller" });
  xhr.onabort = () => finishTransfer(t, { cancelled: true });
  xhr.onload = () => {
    if (xhr.status < 200 || xhr.status >= 300) {
      let msg = xhr.responseText || xhr.statusText || "HTTP " + xhr.status;
      try { const j = JSON.parse(xhr.responseText); if (j.error) msg = j.error; } catch { /* text */ }
      finishTransfer(t, { error: friendlyError(msg, dest.server) });
      return;
    }
    let resp = {};
    try { resp = JSON.parse(xhr.responseText || "{}"); } catch { /* bare path */ }
    if (resp.id) {
      // The browser's part is done: free its slot, follow the agent leg.
      t.cancel = null;
      t.phase = "Transferring to " + dest.server;
      t.bytes = 0;
      t.rate = 0;
      t.percent = 50;
      releaseSlot(t);
      bindHub(t, resp.id, 50);
      return;
    }
    finishTransfer(t, {});
  };
  xhr.send(file);
}

// ----------------------------------------------------------------- transfers

// Every upload, download, copy and move is a transfer record here, rendered in
// the Transfers panel and summarised in the header. Records outlive pane
// navigation (and, via /api/transfers, a page reload). Browser-bound legs
// (uploads to the controller, downloads to the browser) share a small
// concurrency budget so they can't exhaust the browser's per-host connection
// limit; server-side copies/moves hold no browser connection.

const xfer = { list: [], byHub: new Map(), snaps: new Map(), queue: [], active: 0, max: 3, seq: 0, es: null, lastSummary: "" };

const XFER_ICON = { upload: "upload", download: "download", copy: "copy", move: "move" };

function newTransfer(o) {
  const t = Object.assign({
    id: ++xfer.seq, kind: "copy", label: "", srcServer: "", srcPath: "", dstServer: "", dstPath: "",
    state: "queued", bytes: 0, total: 0, rate: 0, percent: 0, startedAt: 0, error: "",
    hubId: "", slot: false, cancel: null, cancellable: false, retry: null, onDone: null, phase: "", legShare: 1, el: null,
  }, o);
  xfer.list.unshift(t);
  renderTransfer(t);
  updateTransfersSummary();
  return t;
}

function enqueue(t, start) {
  t.state = "queued";
  t.start = start;
  t.cancel = () => { xfer.queue = xfer.queue.filter((q) => q !== t); finishTransfer(t, { cancelled: true }); };
  xfer.queue.push(t);
  renderTransfer(t);
  pump();
}

function pump() {
  while (xfer.active < xfer.max && xfer.queue.length) {
    const t = xfer.queue.shift();
    if (t.state !== "queued") continue;
    xfer.active++;
    t.slot = true;
    t.state = "running";
    t.startedAt = Date.now();
    t.cancel = null;
    renderTransfer(t);
    try { t.start(t); } catch (e) { finishTransfer(t, { error: String(e.message || e) }); }
  }
  updateTransfersSummary();
}

function releaseSlot(t) {
  if (!t.slot) return;
  t.slot = false;
  xfer.active = Math.max(0, xfer.active - 1);
  pump();
}

// bindHub attaches a transfer to a server-side progress record.
function bindHub(t, id, offsetPercent = 0) {
  t.hubId = id;
  t.hubOffset = offsetPercent;
  xfer.byHub.set(id, t);
  connectTransferStream();
  // A fast transfer can finish (and its final event arrive) before the POST
  // that started it returns the id: replay the latest snapshot seen for it.
  const seen = xfer.snaps.get(id);
  if (seen) applySnapshot(seen);
}

function finishTransfer(t, { error = "", cancelled = false } = {}) {
  if (t.state === "done" || t.state === "error" || t.state === "cancelled") return;
  t.state = cancelled ? "cancelled" : error ? "error" : "done";
  t.error = cancelled ? "Cancelled" : error;
  t.cancel = null;
  t.cancellable = false;
  if (t.state === "done") { t.percent = 100; if (t.total) t.bytes = t.total; }
  t.finishedAt = Date.now();
  clearTimeout(t.watchdog);
  releaseSlot(t);
  renderTransfer(t);
  updateTransfersSummary();
  if (t.onDone) t.onDone(t.state === "done");
  if (t.state === "error") {
    toast(labelFor(t) + " failed: " + t.error, "error", t.retry ? { action: { label: "Retry", run: () => retryTransfer(t) } } : {});
  } else if (t.state === "done" && t.kind !== "download") {
    announce(labelFor(t) + " finished");
  }
}

function retryTransfer(t) {
  if (!t.retry) return;
  removeTransfer(t);
  t.retry();
}

function removeTransfer(t) {
  xfer.list = xfer.list.filter((x) => x !== t);
  if (t.hubId) xfer.byHub.delete(t.hubId);
  if (t.el) t.el.remove();
  updateTransfersSummary();
}

function labelFor(t) {
  const verb = { upload: "Upload", download: "Download", copy: "Copy", move: "Move" }[t.kind] || "Transfer";
  return verb + " of “" + t.label + "”";
}

function routeFor(t) {
  const src = t.kind === "upload" ? "This browser" : locationLabel(t.srcServer, t.srcPath);
  const dst = t.kind === "download" ? "this browser" : locationLabel(t.dstServer, t.dstPath);
  return src + " → " + dst;
}

// startDownload hands the file to the browser's download manager with a
// tracking id; progress, errors and cancellation come back through the hub.
function startDownload(t) {
  const id = randomHex(16);
  bindHub(t, id);
  t.phase = "Starting";
  const a = h("a", { href: api("/api/download", { server: t.srcServer, path: t.srcPath, track: id }).toString(), download: t.label, hidden: true });
  document.body.appendChild(a);
  a.click();
  a.remove();
  // If the browser never issues the request (e.g. it blocked multiple
  // downloads), say so instead of spinning forever.
  t.watchdog = setTimeout(() => {
    if (t.state === "running" && !t.seen) {
      finishTransfer(t, { error: "The browser didn't start this download — it may be blocking multiple downloads from this page." });
    }
  }, 20000);
  renderTransfer(t);
}

function connectTransferStream() {
  if (xfer.es) return;
  const es = new EventSource(api("/api/transfers/stream").toString());
  xfer.es = es;
  es.onmessage = (ev) => {
    let list;
    try { list = JSON.parse(ev.data); } catch { return; }
    if (Array.isArray(list)) for (const s of list) applySnapshot(s);
  };
  es.onerror = () => {
    // EventSource reconnects on its own; after a hard failure, retry later.
    if (es.readyState === EventSource.CLOSED) {
      xfer.es = null;
      setTimeout(connectTransferStream, 3000);
    }
  };
}

function applySnapshot(s, { adopt = false } = {}) {
  xfer.snaps.delete(s.id);
  xfer.snaps.set(s.id, s);
  if (xfer.snaps.size > 500) xfer.snaps.delete(xfer.snaps.keys().next().value);
  let t = xfer.byHub.get(s.id);
  if (!t) {
    // Unknown ids are usually our own POST still in flight (bindHub replays
    // them). Only transfers that were already running when this page loaded
    // are adopted as new cards.
    if (s.done || !adopt) return;
    t = newTransfer({
      kind: s.kind || "copy", label: s.label || "transfer", srcServer: s.src_server || "", srcPath: s.src_path || "",
      dstServer: s.dst_server || "", dstPath: s.dst_path || "", state: "running", startedAt: Date.parse(s.started_at) || Date.now(),
    });
    t.hubId = s.id;
    t.hubOffset = 0;
    xfer.byHub.set(s.id, t);
  }
  if (t.state !== "running" && t.state !== "queued") return;
  t.seen = true;
  const offset = t.hubOffset || 0;
  if (s.total_bytes > 0) {
    t.total = s.total_bytes;
    t.bytes = s.bytes_done;
    t.percent = offset + ((s.bytes_done / s.total_bytes) * 100 * (100 - offset)) / 100;
  } else if (s.done && !s.error) t.percent = 100;
  if (s.rate_per_sec > 0) t.rate = s.rate_per_sec;
  t.streams = s.active_streams || 0;
  t.cancellable = !!s.cancellable;
  if (t.kind === "download" && t.phase === "Starting") t.phase = "";
  if (s.done) {
    finishTransfer(t, { error: s.cancelled ? "" : s.error || "", cancelled: !!s.cancelled });
    return;
  }
  renderTransfer(t);
  updateTransfersSummary();
}

async function cancelTransfer(t) {
  if (t.state === "queued" || (t.cancel && !t.hubId)) { if (t.cancel) t.cancel(); return; }
  if (t.cancel) { t.cancel(); return; }
  if (!t.hubId || !t.cancellable) return;
  try {
    await postJSON("/api/transfers/cancel", { id: t.hubId });
  } catch (e) {
    toast("Couldn't cancel: " + friendlyError(e), "error");
  }
}

function renderTransfer(t) {
  const list = $("#transfer-list");
  if (!t.el) {
    t.el = h("li", { class: "xfer" });
    list.prepend(t.el);
  }
  const el = t.el;
  el.className = "xfer " + t.state;
  const barFill = h("span", { class: "bar-fill" });
  const running = t.state === "running";
  const indeterminate = running && !t.total && t.percent <= (t.hubOffset || 0);
  barFill.style.width = Math.max(0, Math.min(100, t.state === "done" ? 100 : t.percent)).toFixed(1) + "%";
  const bar = h("div", {
    class: "bar" + (indeterminate ? " indeterminate" : ""), role: "progressbar",
    "aria-label": labelFor(t), "aria-valuemin": "0", "aria-valuemax": "100",
    "aria-valuenow": indeterminate ? null : String(Math.round(t.percent)),
  }, barFill);
  const meta = [];
  if (t.state === "queued") meta.push("Queued");
  else if (running) {
    if (t.phase) meta.push(t.phase);
    if (t.total) meta.push(humanSize(t.bytes) + " of " + humanSize(t.total));
    else if (t.bytes) meta.push(humanSize(t.bytes));
    if (t.rate > 0) meta.push(humanSize(t.rate) + "/s");
    const remaining = t.total ? (t.total - t.bytes) / (t.rate || Infinity) : Infinity;
    if (t.total && t.rate > 0 && isFinite(remaining)) meta.push(fmtDuration(remaining) + " left");
    if (t.streams > 1) meta.push(t.streams + " streams");
    if (!meta.length) meta.push("Working…");
  } else if (t.state === "done") {
    const secs = t.finishedAt && t.startedAt ? (t.finishedAt - t.startedAt) / 1000 : 0;
    meta.push("Done" + (t.total ? " · " + humanSize(t.total) : "") + (secs >= 1 ? " in " + fmtDuration(secs) : ""));
    if (t.kind === "download") meta.push("saved by your browser");
  } else meta.push(t.error || "Failed");
  const actions = h("div", { class: "xfer-actions" });
  const canCancel = t.state === "queued" || (running && (t.cancel || t.cancellable));
  if (canCancel) actions.append(h("button", { class: "icon-btn sm", type: "button", "aria-label": "Cancel " + labelFor(t), title: "Cancel", onclick: () => cancelTransfer(t) }, icon("x")));
  if ((t.state === "error" || t.state === "cancelled") && t.retry) actions.append(h("button", { class: "icon-btn sm", type: "button", "aria-label": "Retry " + labelFor(t), title: "Retry", onclick: () => retryTransfer(t) }, icon("refresh")));
  if (t.state !== "running" && t.state !== "queued") actions.append(h("button", { class: "icon-btn sm", type: "button", "aria-label": "Remove from list", title: "Remove", onclick: () => removeTransfer(t) }, icon("x")));
  if (running && !canCancel) actions.append(h("span", { class: "sr-only", text: "This transfer runs on the controller and can't be cancelled." }));
  const ico = h("span", { class: "xfer-ico", "aria-hidden": "true" }, icon(t.state === "done" ? "check" : t.state === "error" ? "alert" : XFER_ICON[t.kind] || "transfer"));
  el.replaceChildren(
    ico,
    h("div", { class: "xfer-name", title: t.label }, t.label),
    actions,
    h("div", { class: "xfer-route", title: routeFor(t), text: routeFor(t) }),
    bar,
    h("div", { class: "xfer-meta" }, meta.map((m) => h("span", { text: m }))),
  );
}

function updateTransfersSummary() {
  const running = xfer.list.filter((t) => t.state === "running" || t.state === "queued");
  const failed = xfer.list.filter((t) => t.state === "error").length;
  const btn = $("#open-transfers");
  const badge = $("#transfer-count");
  const ring = $("#transfer-ring");
  let totalB = 0, doneB = 0, rate = 0;
  for (const t of running) {
    if (t.total) {
      totalB += t.total;
      doneB += Math.min(t.total, (t.percent / 100) * t.total);
    }
    rate += t.rate || 0;
  }
  const pct = totalB ? (doneB / totalB) * 100 : running.length ? 0 : 100;
  ring.setAttribute("stroke-dashoffset", String(100 - Math.max(2, Math.min(100, pct))));
  btn.classList.toggle("active", running.length > 0);
  btn.classList.toggle("error", failed > 0 && !running.length);
  badge.hidden = !running.length && !failed;
  badge.textContent = String(running.length || failed);
  badge.classList.toggle("error", !running.length && failed > 0);
  const label = running.length ? plural(running.length, "transfer") + " in progress, " + Math.round(pct) + "%" : failed ? plural(failed, "failed transfer") : "Transfers";
  btn.setAttribute("aria-label", label);
  btn.title = label;
  const sum = $("#transfers-summary");
  sum.textContent = running.length
    ? plural(running.length, "active transfer") + (rate > 0 ? " · " + humanSize(rate) + "/s" : "") + (totalB ? " · " + Math.round(pct) + "%" : "")
    : xfer.list.length ? "All done" + (failed ? " · " + failed + " failed" : "") : "Nothing in progress";
  const overall = $("#transfers-overall");
  overall.hidden = !running.length || !totalB;
  $("#overall-fill").style.width = pct.toFixed(1) + "%";
  $("#overall-text").textContent = humanSize(doneB) + " / " + humanSize(totalB);
  $("#transfers-empty").hidden = xfer.list.length > 0;
  // Announce state changes once, not on every progress tick.
  const summary = running.length + "/" + failed;
  if (summary !== xfer.lastSummary && !running.length && xfer.lastSummary && failed === 0) announce("All transfers finished");
  xfer.lastSummary = summary;
}

function openTransfers() {
  const panel = $("#transfers-panel");
  closePreview();
  panel.hidden = false;
  $("#open-transfers").setAttribute("aria-expanded", "true");
  $("#transfers-close").focus();
}

function closeTransfers(refocus = true) {
  const panel = $("#transfers-panel");
  if (panel.hidden) return;
  panel.hidden = true;
  $("#open-transfers").setAttribute("aria-expanded", "false");
  if (refocus) $("#open-transfers").focus();
}

function setupTransfers() {
  $("#open-transfers").addEventListener("click", () => ($("#transfers-panel").hidden ? openTransfers() : closeTransfers()));
  $("#transfers-close").addEventListener("click", () => closeTransfers());
  $("#transfers-clear").addEventListener("click", () => {
    for (const t of xfer.list.slice()) if (t.state !== "running" && t.state !== "queued") removeTransfer(t);
  });
  $("#transfers-panel").addEventListener("keydown", (e) => { if (e.key === "Escape") { e.stopPropagation(); closeTransfers(); } });
  updateTransfersSummary();
  // Re-attach to transfers still running on the controller (e.g. after reload).
  getJSON("/api/transfers").then((res) => {
    for (const s of res.transfers || []) if (!s.done) applySnapshot(s, { adopt: true });
    if ((res.transfers || []).some((s) => !s.done)) connectTransferStream();
  }).catch(() => {});
  setInterval(() => { if (xfer.list.some((t) => t.state === "running")) updateTransfersSummary(); }, 1000);
}

// ----------------------------------------------------------------- preview panel

const preview = { open: false, p: null, item: null, seq: 0 };

function togglePreview(p) {
  if (preview.open && preview.p === p) { closePreview(); return; }
  const it = vis(p).items[p.cursor] || selectedItems(p)[0];
  if (!it) { toast("Select a file or folder to preview", ""); return; }
  openPreview(p, it);
}

const syncPreviewToCursor = debounce((p) => {
  if (!preview.open || preview.p !== p) return;
  const it = vis(p).items[p.cursor];
  if (it && it !== preview.item) openPreview(p, it, { keepFocus: true });
}, 140);

function openPreview(p, item, { keepFocus = false } = {}) {
  const panel = $("#preview-panel");
  closeTransfers(false);
  const wasOpen = preview.open;
  preview.open = true;
  preview.p = p;
  preview.item = item;
  const seq = ++preview.seq;
  for (const q of state.panes) if (q.el) q.el.previewToggle.setAttribute("aria-pressed", String(q === p));
  panel.hidden = false;
  $("#preview-title").textContent = item.name;
  $("#preview-title").title = item.name;
  const pi = $("#preview-panel .preview-icon");
  setIcon(pi, iconNameFor(item));
  pi.classList.toggle("dir", !!item.is_dir);
  const full = fullPath(p, item);

  // Actions
  const actions = $("#preview-actions");
  actions.replaceChildren();
  const regular = !item.is_dir && !item.is_symlink;
  if (item.is_dir) actions.append(h("button", { class: "btn sm primary", type: "button", onclick: () => navigate(p, full) }, icon("arrow-right"), "Open folder"));
  if (regular) actions.append(h("button", { class: "btn sm primary", type: "button", onclick: () => downloadItems(p, [item]) }, icon("download"), "Download"));
  if (regular && isTextFile(item.name)) actions.append(h("button", { class: "btn sm", type: "button", onclick: () => openEditor(p, item) }, icon("edit"), "Edit"));
  if (item.is_symlink) actions.append(h("button", { class: "btn sm primary", type: "button", onclick: () => navigate(p, full) }, icon("folder"), "Open as folder"));
  actions.append(h("button", { class: "btn sm", type: "button", onclick: () => copyPath(p, item) }, icon("copy"), "Copy path"));
  if (regular) actions.append(h("button", { class: "btn sm", type: "button", onclick: () => actChecksum(p, item) }, icon("hash"), "SHA-256"));

  // Metadata (for everything)
  const meta = $("#preview-meta");
  const rows = [
    ["Kind", kindLabel(item)],
    ["Size", item.is_dir ? "—" : humanSize(item.size) + " (" + (item.size || 0).toLocaleString() + " bytes)"],
    ["Modified", fmtDateFull(item.mod_time)],
    ["Permissions", modeString(item.mode, item.is_dir)],
    ["Location", locationLabel(p.server, p.path)],
    ["Path", full],
  ];
  meta.replaceChildren(...rows.flatMap(([k, v]) => [h("dt", { text: k }), h("dd", { text: v, class: k === "Path" || k === "Permissions" ? "mono" : "" })]));

  // Content
  const body = $("#preview-body");
  const placeholder = (ico, text, cls) => h("div", { class: "pv-placeholder" }, icon(ico, cls || ""), h("div", { text }));
  const k = kindOf(item);
  if (item.is_dir) body.replaceChildren(placeholder("folder", "Folder", "dir"));
  else if (item.is_symlink) body.replaceChildren(placeholder("link", "Symbolic link — the agent resolves it when you open it (links leaving the allowed file roots are refused)."));
  else if (k === "image" && IMAGE_PREVIEW_EXTS.has(extOf(item.name))) {
    if ((item.size || 0) > 25 * 1024 * 1024) body.replaceChildren(placeholder("file-image", "Too large to preview (" + humanSize(item.size) + ")"));
    else {
      const img = h("img", { alt: item.name, decoding: "async" });
      img.addEventListener("error", () => { if (seq === preview.seq) body.replaceChildren(placeholder("alert", "This image couldn't be displayed.")); });
      img.src = api("/api/preview", { server: p.server, path: full, kind: "image" }).toString();
      body.replaceChildren(h("div", { class: "pv-image" }, img));
    }
  } else if (k === "media" || k === "archive" || (k === "image" && !IMAGE_PREVIEW_EXTS.has(extOf(item.name)) && extOf(item.name) !== "svg")) {
    body.replaceChildren(placeholder(iconNameFor(item), "No preview for " + kindLabel(item).toLowerCase() + "s"));
  } else {
    body.replaceChildren(h("div", { class: "pv-placeholder" }, h("div", { class: "spinner", role: "progressbar", "aria-label": "Loading preview" })));
    getJSON("/api/preview", { server: p.server, path: full, kind: "text" }).then((res) => {
      if (seq !== preview.seq) return;
      if (res.binary) { body.replaceChildren(placeholder(iconNameFor(item), "Binary file — no text preview")); return; }
      const code = h("code");
      const lang = langFor(item.name);
      // Highlight moderately sized text; everything is escaped either way.
      if (res.content.length <= 150000 && lang !== "text") code.innerHTML = highlightCode(res.content, lang);
      else code.textContent = res.content;
      const kids = [h("pre", { tabindex: "0", "aria-label": "Contents of " + item.name }, code)];
      if (res.truncated) kids.push(h("div", { class: "pv-note", text: "Showing the first " + humanSize(res.content.length) + " of " + humanSize(res.size) + "." }));
      if (!res.content.length) kids.splice(0, 1, placeholder("file-text", "Empty file"));
      body.replaceChildren(...kids);
    }).catch((e) => {
      if (seq !== preview.seq) return;
      body.replaceChildren(placeholder("alert", "Preview unavailable: " + friendlyError(e, p.server)));
    });
  }
  if (!wasOpen && !keepFocus) $("#preview-close").focus();
}

function closePreview() {
  if (!preview.open) return;
  preview.open = false;
  preview.seq++;
  $("#preview-panel").hidden = true;
  $("#preview-body").replaceChildren();
  for (const q of state.panes) if (q.el) q.el.previewToggle.setAttribute("aria-pressed", "false");
  const p = preview.p;
  preview.p = null;
  preview.item = null;
  if (p && p.el && document.activeElement && (document.activeElement === document.body || $("#preview-panel").contains(document.activeElement))) p.el.grid.focus();
}

function setupPreview() {
  $("#preview-close").addEventListener("click", () => { const p = preview.p; closePreview(); if (p && p.el) p.el.grid.focus(); });
  $("#preview-panel").addEventListener("keydown", (e) => {
    if (e.key === "Escape") { e.stopPropagation(); const p = preview.p; closePreview(); if (p && p.el) p.el.grid.focus(); }
  });
}

// ----------------------------------------------------------------- command palette

// fuzzyScore returns a score (higher is better) and the matched indices for a
// subsequence match of q in s, or null.
function fuzzyScore(q, s) {
  if (!q) return { score: 0, hits: [] };
  const ls = s.toLowerCase();
  const direct = ls.indexOf(q);
  if (direct >= 0) {
    const hits = [];
    for (let i = 0; i < q.length; i++) hits.push(direct + i);
    return { score: 100 - direct + (direct === 0 ? 50 : 0) - s.length * 0.1, hits };
  }
  let si = 0, score = 0, prev = -2;
  const hits = [];
  for (let qi = 0; qi < q.length; qi++) {
    const c = q[qi];
    const found = ls.indexOf(c, si);
    if (found < 0) return null;
    score += found === prev + 1 ? 5 : 1;
    if (found === 0 || /[\s._\-/]/.test(ls[found - 1] || "")) score += 3;
    hits.push(found);
    prev = found;
    si = found + 1;
  }
  return { score: score - s.length * 0.05, hits };
}

function highlightHits(text, hits) {
  const frag = document.createDocumentFragment();
  const set = new Set(hits);
  let buf = "";
  let inMark = false;
  const flush = () => {
    if (!buf) return;
    frag.append(inMark ? h("mark", { text: buf }) : document.createTextNode(buf));
    buf = "";
  };
  for (let i = 0; i < text.length; i++) {
    const m = set.has(i);
    if (m !== inMark) { flush(); inMark = m; }
    buf += text[i];
  }
  flush();
  return frag;
}

function paletteCommands() {
  const p = activePane();
  const cmds = [
    { label: "Go to Files", icon: "folder", run: () => showView("files") },
    { label: "Go to Fleet overview", icon: "server", run: () => showView("overview") },
    { label: "Show transfers", icon: "transfer", run: openTransfers },
    { label: "Keyboard shortcuts", icon: "keyboard", hint: "?", run: showShortcuts },
    { label: "Toggle light / dark theme", icon: "moon", run: toggleTheme },
    { label: "Theme: follow system", icon: "sun", run: () => setTheme("") },
    { label: "Add pane", icon: "columns", run: () => addPane() },
  ];
  if (p) {
    cmds.push(
      { label: "New folder", icon: "folder-plus", hint: "Shift+N", run: () => promptMkdir(p) },
      { label: "New file", icon: "file-plus", run: () => promptNewFile(p) },
      { label: "Upload files…", icon: "upload", run: () => p.el.fileInput.click() },
      { label: "Go to path…", icon: "arrow-right", hint: "G", run: () => { showView("files"); startPathEdit(p); } },
      { label: "Go up one level", icon: "arrow-up", hint: "Backspace", run: () => goUp(p) },
      { label: p.hidden ? "Hide hidden files" : "Show hidden files", icon: "eye", hint: ".", run: () => toggleHidden(p) },
      { label: p.view === "list" ? "Switch to icon view" : "Switch to list view", icon: p.view === "list" ? "grid" : "list", hint: "V", run: () => setView(p, p.view === "list" ? "icons" : "list") },
      { label: "Refresh", icon: "refresh", hint: "R", run: () => loadListing(p, { keepScroll: true }) },
      { label: "Select all", icon: "check", hint: MOD + "+A", run: () => selectAll(p) },
      { label: "Paste", icon: "clipboard", hint: MOD + "+V", run: () => pasteInto(p) },
    );
    if (state.panes.length > 1) cmds.push({ label: "Close pane", icon: "x", run: () => removePane(p) });
  }
  return cmds;
}

function openPalette() {
  if (modalOpen()) return;
  const p = activePane();
  const inputId = "palette-input", listId = "palette-list";
  const input = h("input", {
    id: inputId, type: "text", role: "combobox", "aria-expanded": "true", "aria-controls": listId,
    "aria-autocomplete": "list", "aria-label": "Search files, servers and commands", placeholder: "Search files in this folder, servers, commands…",
    spellcheck: "false", autocomplete: "off",
  });
  const list = h("ul", { id: listId, class: "palette-list", role: "listbox", "aria-label": "Results" });
  let options = [];
  let active = 0;
  const render = () => {
    const q = input.value.trim().toLowerCase();
    const groups = [];
    const push = (name, arr) => { if (arr.length) groups.push({ name, arr }); };
    const score = (label) => fuzzyScore(q, label);
    // Files in the active pane's folder (quick-open).
    if (p && p.items.length) {
      const hits = [];
      const src = vis(p).items;
      const limit = q ? src.length : Math.min(src.length, 8);
      for (let i = 0; i < limit && hits.length < 400; i++) {
        const it = src[i];
        const m = q ? score(it.name) : { score: 0, hits: [] };
        if (m) hits.push({ label: it.name, hits: m.hits, score: m.score, icon: iconNameFor(it), cls: it.is_dir ? "dir" : "", hint: it.is_dir ? "folder" : humanSize(it.size), run: () => { showView("files"); const idx = vis(p).index.get(it.name); if (idx !== undefined) { selectOnly(p, idx); p.el.grid.focus(); } openItem(p, it); } });
      }
      hits.sort((a, b) => b.score - a.score);
      push("In " + baseName(p.path, p.pathStyle), hits.slice(0, q ? 12 : 6));
    }
    const servers = [{ name: "", label: "Local" }, ...state.servers.map((s) => ({ name: s.name, label: s.name + (s.reachable ? "" : " (offline)"), off: !s.reachable }))];
    push("Open in this pane", servers.map((s) => {
      const m = score(s.label);
      return m && !s.off ? { label: s.label, hits: m.hits, score: m.score, icon: s.name ? "server" : "laptop", hint: "server", run: () => { showView("files"); const q2 = activePane(); applyPaneSource(q2, s.name); if (q2.el) { q2.el.server.value = s.name; renderSourceChrome(q2); } loadListing(q2); saveLayout(); } } : null;
    }).filter(Boolean).sort((a, b) => b.score - a.score).slice(0, q ? 8 : 5));
    const recent = (store.get("recent", []) || []).filter((r) => !(p && r.server === p.server && r.path === p.path));
    push("Recent locations", recent.map((r) => {
      const label = locationLabel(r.server, r.path);
      const m = score(label);
      return m ? { label, hits: m.hits, score: m.score, icon: "history", run: () => { showView("files"); const q2 = activePane(); if (q2.server !== r.server) { applyPaneSource(q2, r.server, { path: r.path }); if (q2.el) { q2.el.server.value = r.server; renderSourceChrome(q2); } loadListing(q2); } else navigate(q2, r.path); } } : null;
    }).filter(Boolean).slice(0, q ? 6 : 4));
    push("Commands", paletteCommands().map((c) => {
      const m = score(c.label);
      return m ? Object.assign({}, c, { hits: m.hits, score: m.score }) : null;
    }).filter(Boolean).sort((a, b) => (q ? b.score - a.score : 0)).slice(0, q ? 10 : 20));

    list.replaceChildren();
    options = [];
    for (const g of groups) {
      list.append(h("li", { class: "palette-group", role: "presentation", text: g.name }));
      for (const o of g.arr) {
        const id = "po-" + options.length;
        const li = h("li", { id, class: "palette-opt", role: "option", "aria-selected": "false" },
          icon(o.icon || "cursor", o.cls || ""),
          h("span", { class: "po-label" }, highlightHits(o.label, o.hits || [])),
          o.hint ? h("span", { class: "po-hint", text: o.hint }) : null);
        const index = options.length;
        li.addEventListener("pointerdown", (e) => e.preventDefault());
        li.addEventListener("click", () => choose(index));
        li.addEventListener("pointermove", () => { if (active !== index) setActiveOpt(index); });
        list.append(li);
        options.push({ o, li });
      }
    }
    if (!options.length) list.append(h("li", { class: "palette-empty", role: "presentation", text: "No matches" }));
    setActiveOpt(0);
  };
  const setActiveOpt = (i) => {
    if (!options.length) { input.removeAttribute("aria-activedescendant"); return; }
    active = (i + options.length) % options.length;
    options.forEach((x, k) => x.li.setAttribute("aria-selected", String(k === active)));
    input.setAttribute("aria-activedescendant", options[active].li.id);
    options[active].li.scrollIntoView({ block: "nearest" });
  };
  const choose = (i) => {
    const pick = options[i];
    if (!pick) return;
    closeModal(null);
    setTimeout(() => pick.o.run(), 0);
  };
  input.addEventListener("input", render);
  const foot = h("div", { class: "palette-foot", "aria-hidden": "true" },
    h("span", null, h("kbd", { text: "↑" }), " ", h("kbd", { text: "↓" }), " navigate"),
    h("span", null, h("kbd", { text: "Enter" }), " open"),
    h("span", null, h("kbd", { text: "Esc" }), " close"));
  showModal([h("div", { class: "palette-input" }, icon("search"), input), list, foot], {
    className: "palette",
    initialFocus: input,
    onKey: (e) => {
      if (e.key === "ArrowDown") { e.preventDefault(); setActiveOpt(active + 1); return true; }
      if (e.key === "ArrowUp") { e.preventDefault(); setActiveOpt(active - 1); return true; }
      if (e.key === "Enter") { e.preventDefault(); choose(active); return true; }
      if (e.key === "Tab") { e.preventDefault(); return true; }
      return false;
    },
  });
  $("#modal").setAttribute("aria-label", "Command palette");
  render();
}

// ----------------------------------------------------------------- shortcuts

function showShortcuts() {
  const k = (...keys) => keys.map((x) => h("kbd", { text: x }));
  const section = (title, rows) => h("section", null, h("h3", { text: title }),
    h("dl", null, rows.flatMap(([label, keys]) => [h("dt", { text: label }), h("dd", null, keys)])));
  const id = "dlg-" + ++dialogSeq;
  showModal([
    h("h2", { id, text: "Keyboard shortcuts" }),
    h("div", { class: "shortcut-grid" },
      section("Navigate", [
        ["Move through items", k("↑", "↓", "←", "→")],
        ["First / last item", k("Home", "End")],
        ["Open folder / preview file", k("Enter")],
        ["Up one level", k("Backspace")],
        ["Back / forward", k("Alt", "←", "→")],
        ["Go to path", k("G")],
        ["Filter this folder", k("/")],
        ["Next / previous pane", k("F6")],
      ]),
      section("Select", [
        ["Select item", k("Space")],
        ["Extend selection", k("Shift", "↑↓")],
        ["Select range", k("Shift", "Click")],
        ["Toggle item", k(MOD, "Click")],
        ["Select all", k(MOD, "A")],
        ["Clear selection", k("Esc")],
      ]),
      section("Act", [
        ["Copy", k(MOD, "C")],
        ["Cut", k(MOD, "X")],
        ["Paste into this pane", k(MOD, "V")],
        ["Rename", k("F2")],
        ["Delete", k(IS_MAC ? "⌘⌫" : "Delete")],
        ["New folder", k("Shift", "N")],
        ["Download file", k(MOD, "Enter")],
        ["Edit text file", k("E")],
        ["Context menu", k("Shift", "F10")],
      ]),
      section("View", [
        ["Command palette", k(MOD, "K")],
        ["Preview panel", k("P")],
        ["List / icon view", k("V")],
        ["Show hidden files", k(".")],
        ["Refresh", k("R")],
        ["This help", k("?")],
      ]),
    ),
    h("div", { class: "modal-actions" }, h("button", { class: "btn primary", type: "button", text: "Close", onclick: () => closeModal(null) })),
  ], { className: "wide", labelledBy: id });
}

// ----------------------------------------------------------------- views / theme

function showView(view) {
  if (view !== "files" && view !== "overview") return;
  state.view = view;
  for (const tab of $$(".view-tab")) {
    const on = tab.dataset.view === view;
    tab.setAttribute("aria-selected", String(on));
    tab.tabIndex = on ? 0 : -1;
  }
  $("#view-files").hidden = view !== "files";
  $("#view-overview").hidden = view !== "overview";
  document.title = view === "files" ? "Cenvero Fleet — Files" : "Cenvero Fleet — Fleet overview";
  if (location.hash !== "#" + view) history.replaceState(null, "", location.pathname + "#" + view);
  if (view === "overview") { closePreview(); loadOverview(); scheduleOverview(); }
  else {
    stopOverviewTimer();
    const p = activePane();
    if (p) requestAnimationFrame(() => { renderRows(p); });
  }
}

function currentTheme() {
  const set = document.documentElement.getAttribute("data-theme");
  if (set === "light" || set === "dark") return set;
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function setTheme(theme) {
  if (theme === "light" || theme === "dark") {
    document.documentElement.setAttribute("data-theme", theme);
    try { localStorage.setItem("fleet.theme", theme); } catch { /* ignore */ }
  } else {
    document.documentElement.removeAttribute("data-theme");
    try { localStorage.removeItem("fleet.theme"); } catch { /* ignore */ }
  }
  renderThemeToggle();
}

function toggleTheme() { setTheme(currentTheme() === "dark" ? "light" : "dark"); }

function renderThemeToggle() {
  const dark = currentTheme() === "dark";
  const btn = $("#theme-toggle");
  btn.setAttribute("aria-label", dark ? "Switch to light theme" : "Switch to dark theme");
  btn.title = (dark ? "Light" : "Dark") + " theme";
  $("#theme-icon").setAttribute("href", dark ? "#i-sun" : "#i-moon");
}

function applyResponsive() {
  const mobile = window.matchMedia("(max-width: 760px)").matches;
  const changed = mobile !== state.mobile;
  state.mobile = mobile;
  document.body.classList.toggle("is-mobile", mobile);
  measureRowHeight();
  if (changed) {
    for (const p of state.panes) { renderCrumbs(p); if (p.el) p.el.mode = null; }
    setActivePane(activePane());
  }
  for (const p of state.panes) renderRows(p);
}

// ----------------------------------------------------------------- global keyboard

function isTypingTarget(el) {
  if (!el) return false;
  const tag = (el.tagName || "").toLowerCase();
  return tag === "input" || tag === "textarea" || tag === "select" || el.isContentEditable;
}

function setupKeyboard() {
  document.addEventListener("keydown", (e) => {
    if (editor.open) return;
    if (modalOpen()) return; // the dialog traps its own keys
    if (menuOpen()) return;  // the menu handles its own keys
    if (modKey(e) && !e.altKey && e.key.toLowerCase() === "k") { e.preventDefault(); openPalette(); return; }
    if (isTypingTarget(e.target)) return;
    if (e.key === "?" || (e.shiftKey && e.key === "/")) { e.preventDefault(); showShortcuts(); return; }
    if (state.view !== "files") {
      if (e.key === "Escape") { closeTransfers(false); }
      return;
    }
    const p = activePane();
    if (!p || !p.el) return;
    if (e.key === "Escape" && (!$("#transfers-panel").hidden || preview.open) && !p.sel.size) {
      closeTransfers(false); closePreview(); p.el.grid.focus(); return;
    }
    if (e.key === "/" || (modKey(e) && e.key.toLowerCase() === "f")) { e.preventDefault(); p.el.filterInput.focus(); p.el.filterInput.select(); return; }
    if (modKey(e) && e.key.toLowerCase() === "l") { e.preventDefault(); startPathEdit(p); return; }
    if (e.key === "F6") {
      e.preventDefault();
      const n = state.panes.length;
      const next = state.panes[(state.active + (e.shiftKey ? n - 1 : 1)) % n];
      setActivePane(next, { focus: true });
      announce("Pane " + paneNumber(next) + ": " + locationLabel(next.server, next.path));
      return;
    }
    // Keys pressed outside any control (e.g. after clicking empty space) act
    // on the active pane as if its list had focus.
    const inPaneControl = e.target.closest && e.target.closest("button, a, [role=menu]");
    if (!inPaneControl && e.target !== p.el.grid) onGridKey(p, e);
  });
}

// ----------------------------------------------------------------- fleet overview (read-only)

const ov = { data: null, loading: false, timer: 0, auto: true, sort: { key: "name", dir: 1 }, status: "all", filter: "", lastOk: 0, tick: 0 };
const OV_REFRESH_MS = 10000;
const STATUS_RANK = { online: 0, pending: 1, "reconcile-required": 2, "install-failed": 3, offline: 4 };

async function loadOverview() {
  if (ov.loading) return;
  ov.loading = true;
  try {
    ov.data = await getJSON("/api/overview");
    ov.lastOk = Date.now();
    ov.error = null;
  } catch (e) {
    ov.error = e;
  }
  ov.loading = false;
  renderOverview();
  renderAlertBadge();
}

function scheduleOverview() {
  stopOverviewTimer();
  if (!ov.auto) return;
  ov.timer = setInterval(() => {
    if (document.hidden || state.view !== "overview") return;
    loadOverview();
  }, OV_REFRESH_MS);
  ov.tick = setInterval(renderOverviewUpdated, 1000);
}

function stopOverviewTimer() {
  clearInterval(ov.timer);
  clearInterval(ov.tick);
  ov.timer = 0;
  ov.tick = 0;
}

function renderOverviewUpdated() {
  const el = $("#ov-updated");
  if (ov.error && !ov.data) el.textContent = "Couldn't load";
  else if (ov.lastOk) el.textContent = "Updated " + fmtRelative(new Date(ov.lastOk).toISOString()) + (ov.auto ? " · refreshes every 10s" : " · auto-refresh paused");
}

function renderAlertBadge() {
  const badge = $("#alert-badge");
  if (!ov.data) return;
  const a = ov.data.summary.alerts;
  const n = a.critical + a.warning;
  badge.hidden = n === 0;
  badge.textContent = String(n);
  badge.classList.toggle("warn", a.critical === 0);
  badge.setAttribute("aria-label", plural(n, "open alert"));
  $("#tab-overview").setAttribute("aria-label", "Fleet overview" + (n ? ", " + plural(n, "open alert") : ""));
}

function metricClass(v) { return v >= 90 ? "crit" : v >= 75 ? "warn" : ""; }

function meter(label, value) {
  if (value === undefined || value === null || isNaN(value)) return h("span", { class: "muted", text: "—" });
  const v = Math.max(0, Math.min(100, value));
  const fill = h("span", { class: "bar-fill " + metricClass(v) });
  fill.style.width = v.toFixed(1) + "%";
  return h("div", { class: "meter", role: "meter", "aria-label": label, "aria-valuemin": "0", "aria-valuemax": "100", "aria-valuenow": String(Math.round(v)), "aria-valuetext": Math.round(v) + "%" },
    h("div", { class: "bar" }, fill), h("span", { class: "meter-val", text: Math.round(v) + "%" }));
}

function serverMatches(s, q) {
  if (!q) return true;
  const terms = q.toLowerCase().split(/\s+/).filter(Boolean);
  return terms.every((term) => {
    const kv = term.split("=");
    if (kv.length === 2 && kv[0]) {
      const val = s.tags ? s.tags[kv[0]] : undefined;
      return val !== undefined && (!kv[1] || String(val).toLowerCase().includes(kv[1]));
    }
    const hay = [s.name, s.status, s.mode, s.os, s.arch, s.address, s.agent_version,
      ...Object.entries(s.tags || {}).map(([k, v]) => k + "=" + v)].join(" ").toLowerCase();
    return hay.includes(term);
  });
}

function overviewRows() {
  const d = ov.data;
  if (!d) return [];
  const q = ov.filter.trim();
  let rows = d.servers.filter((s) => serverMatches(s, q));
  if (ov.status === "online") rows = rows.filter((s) => s.status === "online");
  else if (ov.status === "offline") rows = rows.filter((s) => s.status !== "online");
  else if (ov.status === "alerts") rows = rows.filter((s) => s.alerts.critical + s.alerts.warning + s.alerts.info > 0);
  const { key, dir } = ov.sort;
  const val = (s) => {
    const m = s.metrics || {};
    switch (key) {
      case "status": return STATUS_RANK[s.status] ?? 9;
      case "mode": return s.mode || "";
      case "os": return (s.os || "") + "/" + (s.arch || "");
      case "cpu": return m.cpu_percent ?? -1;
      case "mem": return m.memory_percent ?? -1;
      case "disk": return m.disk_percent ?? -1;
      case "seen": return Date.parse(s.last_seen) || 0;
      case "alerts": return s.alerts.critical * 10000 + s.alerts.warning * 100 + s.alerts.info;
      default: return s.name;
    }
  };
  rows.sort((a, b) => {
    const va = val(a), vb = val(b);
    const c = typeof va === "number" ? va - vb : COLLATOR.compare(String(va), String(vb));
    return (c || COLLATOR.compare(a.name, b.name)) * dir;
  });
  return rows;
}

function statusLabel(s) {
  return { online: "Online", offline: "Offline", pending: "Pending", "install-failed": "Install failed", "reconcile-required": "Needs reconcile" }[s] || s;
}

function renderOverview() {
  renderOverviewUpdated();
  const d = ov.data;
  const summary = $("#ov-summary");
  const notice = $("#ov-notice");
  const body = $("#ov-body");
  const empty = $("#ov-empty");
  if (!d) {
    body.replaceChildren();
    summary.replaceChildren();
    empty.hidden = false;
    empty.textContent = ov.error ? "Couldn't load the fleet: " + friendlyError(ov.error) : "Loading…";
    return;
  }
  // Summary cards double as quick filters.
  const card = (label, value, cls, foot, status, ico) => {
    const pressed = status && ov.status === status;
    const el = h(status ? "button" : "div", { class: "stat", type: status ? "button" : null, "aria-pressed": status ? String(pressed) : null },
      h("span", { class: "stat-label" }, icon(ico), label),
      h("span", { class: "stat-value " + (cls || ""), text: String(value) }),
      foot ? h("span", { class: "stat-foot" }, foot) : null);
    if (status) el.addEventListener("click", () => setOverviewStatus(pressed ? "all" : status));
    return el;
  };
  const a = d.summary.alerts;
  summary.replaceChildren(
    card("Servers", d.summary.total, "", d.summary.other ? plural(d.summary.other, "pending / other") : "managed by this controller", null, "server"),
    card("Online", d.summary.online, d.summary.online ? "ok" : "", null, "online", "activity"),
    card("Offline", d.summary.offline + d.summary.other, d.summary.offline ? "bad" : "", null, "offline", "alert"),
    card("Open alerts", d.alerts_restricted ? "—" : a.critical + a.warning + a.info, a.critical ? "bad" : a.warning ? "warn" : "",
      d.alerts_restricted ? "hidden by your token" : [h("span", { class: "badge crit", text: a.critical + " critical" }), h("span", { class: "badge warn", text: a.warning + " warning" })], "alerts", "alert"),
  );
  const notes = [];
  if (ov.error) notes.push("Showing data from " + fmtRelative(new Date(ov.lastOk).toISOString()) + " — refresh failed: " + friendlyError(ov.error));
  if (d.alerts_restricted) notes.push("Alerts are hidden: the token this UI was started with can't run `fleet alerts`.");
  if (d.tags_restricted) notes.push("Tags are hidden: the token this UI was started with can't run `fleet tag`.");
  notice.hidden = !notes.length;
  notice.replaceChildren(...notes.map((n) => callout("warn", "info", n)));

  for (const th of $$("#ov-table th[aria-sort]")) {
    const key = $("button", th) ? $("button", th).dataset.sort : "";
    th.setAttribute("aria-sort", key === ov.sort.key ? (ov.sort.dir > 0 ? "ascending" : "descending") : "none");
  }
  const rows = overviewRows();
  body.replaceChildren(...rows.map((s) => {
    const m = s.metrics || null;
    const alertsCell = h("td", { class: "c-alerts", "data-label": "Alerts" });
    const total = s.alerts.critical + s.alerts.warning + s.alerts.info;
    if (!total) alertsCell.append(h("span", { class: "muted", text: d.alerts_restricted ? "—" : "None" }));
    if (s.alerts.critical) alertsCell.append(h("span", { class: "badge crit", text: s.alerts.critical + " critical" }), " ");
    if (s.alerts.warning) alertsCell.append(h("span", { class: "badge warn", text: s.alerts.warning + " warning" }), " ");
    if (s.alerts.info) alertsCell.append(h("span", { class: "badge info", text: s.alerts.info + " info" }));
    const tags = Object.entries(s.tags || {}).sort(([x], [y]) => x.localeCompare(y));
    const browse = h("button", {
      class: "btn sm", type: "button", disabled: !s.reachable,
      title: s.reachable ? "Browse " + s.name + " in the active pane" : s.name + " is not reachable",
      "aria-label": "Browse files on " + s.name,
      onclick: (e) => { e.stopPropagation(); openServerInFiles(s.name); },
    }, icon("folder"), "Browse");
    const tr = h("tr", { class: s.reachable ? "" : "unreachable", dataset: { server: s.name } },
      h("td", { class: "c-status", "data-label": "Status" }, h("span", { class: "status-cell" }, h("span", { class: "dot " + (s.status === "online" ? "online" : s.status === "offline" ? "offline" : "other") }), statusLabel(s.status))),
      h("td", { class: "c-name" }, h("div", { class: "srv-name" },
        h("b", { text: s.name }),
        h("span", { class: "srv-sub", text: [s.address, s.agent_version ? "agent " + s.agent_version : ""].filter(Boolean).join(" · ") || "—", title: s.last_error || "" }))),
      h("td", { class: "c-mode", "data-label": "Mode", text: s.mode || "—" }),
      h("td", { class: "c-os", "data-label": "OS", text: s.os ? s.os + (s.arch ? "/" + s.arch : "") : "—" }),
      h("td", { class: "c-metric", "data-label": "CPU" }, meter(s.name + " CPU", m ? m.cpu_percent : null)),
      h("td", { class: "c-metric", "data-label": "Memory" }, meter(s.name + " memory", m ? m.memory_percent : null)),
      h("td", { class: "c-metric", "data-label": "Disk" }, meter(s.name + " disk", m ? m.disk_percent : null)),
      h("td", { class: "c-seen", "data-label": "Last seen", title: fmtDateFull(s.last_seen) + (s.last_error ? "\n" + s.last_error : ""), text: fmtRelative(s.last_seen) }),
      h("td", { class: "c-tags", "data-label": "Tags" }, tags.length ? h("div", { class: "tags" }, tags.map(([k, v]) => h("span", { class: "tag" }, h("b", { text: k }), "=" + v))) : h("span", { class: "muted", text: d.tags_restricted ? "—" : "No tags" })),
      alertsCell,
      h("td", { class: "c-open" }, browse),
    );
    tr.addEventListener("click", () => { if (s.reachable) openServerInFiles(s.name); });
    return tr;
  }));
  empty.hidden = rows.length > 0;
  empty.textContent = d.servers.length ? "No servers match the current filter." : "No servers yet — add one with `fleet server add`.";

  const list = $("#ov-alerts");
  if (d.alerts_restricted) list.replaceChildren(h("li", { class: "alert-none", text: "Hidden by the token this UI was started with." }));
  else if (!d.alerts.length) list.replaceChildren(h("li", { class: "alert-none", text: "No open alerts. " }));
  else {
    const sevRank = { critical: 0, warning: 1, info: 2 };
    const alerts = d.alerts.slice().sort((x, y) => (sevRank[x.severity] ?? 3) - (sevRank[y.severity] ?? 3) || (Date.parse(y.updated_at) || 0) - (Date.parse(x.updated_at) || 0));
    list.replaceChildren(...alerts.map((al) => h("li", { class: "alert-item " + al.severity },
      icon(al.severity === "info" ? "info" : "alert"),
      h("div", { class: "alert-body" },
        h("div", { class: "alert-msg", text: al.message }),
        h("div", { class: "alert-meta" },
          h("span", { text: al.severity.toUpperCase() }),
          al.server ? h("span", { text: al.server }) : null,
          al.code ? h("span", { text: al.code }) : null,
          h("span", { text: "since " + fmtRelative(al.created_at), title: fmtDateFull(al.created_at) }),
          al.occurrences > 1 ? h("span", { text: al.occurrences + "×" }) : null)),
    )));
  }
}

function setOverviewStatus(status) {
  ov.status = status;
  for (const b of $$("#ov-status button")) {
    const on = b.dataset.status === status;
    b.setAttribute("aria-checked", String(on));
    b.tabIndex = on ? 0 : -1;
  }
  renderOverview();
}

// openServerInFiles switches to Files and shows the server in the active pane.
function openServerInFiles(name) {
  showView("files");
  let p = activePane();
  if (!p) return;
  if (p.server !== name) {
    applyPaneSource(p, name);
    if (p.el) { p.el.server.value = name; renderSourceChrome(p); p.el.filterInput.value = ""; }
    loadListing(p);
    saveLayout();
  }
  setActivePane(p, { focus: true });
  toast("Browsing " + name + " in pane " + paneNumber(p), "success");
}

function setupOverview() {
  const filter = $("#ov-filter");
  filter.addEventListener("input", debounce(() => { ov.filter = filter.value; renderOverview(); }, 100));
  filter.addEventListener("keydown", (e) => { if (e.key === "Escape" && filter.value) { e.stopPropagation(); filter.value = ""; ov.filter = ""; renderOverview(); } });
  const seg = $("#ov-status");
  for (const b of $$("button", seg)) b.addEventListener("click", () => setOverviewStatus(b.dataset.status));
  seg.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    const btns = $$("button", seg);
    const i = btns.findIndex((b) => b.getAttribute("aria-checked") === "true");
    const next = btns[(i + (e.key === "ArrowRight" ? 1 : btns.length - 1)) % btns.length];
    setOverviewStatus(next.dataset.status);
    next.focus();
  });
  $("#ov-refresh").addEventListener("click", loadOverview);
  const auto = $("#ov-auto");
  auto.addEventListener("click", () => {
    ov.auto = !ov.auto;
    auto.setAttribute("aria-pressed", String(ov.auto));
    setIcon(auto.querySelector("svg"), ov.auto ? "pause" : "play");
    if (ov.auto) { scheduleOverview(); loadOverview(); } else { stopOverviewTimer(); renderOverviewUpdated(); }
  });
  for (const b of $$("#ov-table th button")) {
    b.addEventListener("click", () => {
      const key = b.dataset.sort;
      if (ov.sort.key === key) ov.sort.dir = -ov.sort.dir;
      else ov.sort = { key, dir: key === "name" || key === "os" || key === "mode" ? 1 : -1 };
      renderOverview();
    });
  }
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && state.view === "overview" && ov.auto && Date.now() - ov.lastOk > OV_REFRESH_MS) loadOverview();
  });
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
  // Escape the ENTIRE untrusted document first. From this point on every slice
  // is inert text; the only literal tags introduced are our fixed <span>s.
  const safe = escHTML(src);
  let out = "";
  let i = 0;
  const n = safe.length;
  while (i < n) {
    if (safe.startsWith("&lt;!--", i)) {
      const end = safe.indexOf("--&gt;", i);
      const stop = end < 0 ? n : end + 6;
      out += spanEscaped("cm", safe.slice(i, stop));
      i = stop;
      continue;
    }
    if (safe.startsWith("&lt;", i)) {
      const end = safe.indexOf("&gt;", i);
      const stop = end < 0 ? n : end + 4;
      out += hlEscapedTag(safe.slice(i, stop));
      i = stop;
      continue;
    }
    const lt = safe.indexOf("&lt;", i);
    const stop = lt < 0 ? n : lt;
    out += safe.slice(i, stop);
    i = stop;
  }
  return out;
}

function spanEscaped(cls, safeText) {
  return '<span class="hl-' + cls + '">' + safeText + "</span>";
}

function hlEscapedTag(tag) {
  let out = "&lt;";
  let body = tag.slice(4, tag.endsWith("&gt;") ? -4 : undefined);
  const closing = tag.endsWith("&gt;") ? "&gt;" : "";
  const nameMatch = body.match(/^\/?[A-Za-z0-9:-]+/);
  if (nameMatch) {
    out += spanEscaped("ky", nameMatch[0]);
    body = body.slice(nameMatch[0].length);
  }
  let i = 0;
  while (i < body.length) {
    const q = body[i];
    if (q === '"' || q === "'") {
      let end = i + 1;
      while (end < body.length && body[end] !== q) end++;
      if (end < body.length) end++;
      out += spanEscaped("st", body.slice(i, end));
      i = end;
      continue;
    }
    const attr = body.slice(i).match(/^([A-Za-z_:][A-Za-z0-9_.:-]*)(\s*=)/);
    if (attr) {
      out += spanEscaped("nm", attr[1]) + attr[2];
      i += attr[0].length;
      continue;
    }
    out += body[i];
    i++;
  }
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
  p: null,
  item: null,
  original: "",
  lang: "text",
  dirty: false,
  lastFocus: null,
};

async function openEditor(p, item) {
  const full = fullPath(p, item);
  const overlay = $("#editor-overlay");
  const ta = $("#ed-input");
  editor.lastFocus = document.activeElement;
  $("#ed-name").textContent = item.name;
  $("#ed-name").title = locationLabel(p.server, full);
  $("#ed-lang").textContent = langLabel(item.name);
  $("#ed-meta").textContent = "Loading…";
  ta.value = "";
  ta.disabled = true;
  setDirty(false);
  highlightEditor("", langFor(item.name));
  overlay.hidden = false;
  closeMenu(false);
  editor.open = true;
  editor.p = p;
  editor.item = item;
  editor.lang = langFor(item.name);

  let data;
  try {
    data = await getJSON("/api/read", { server: p.server, path: full });
  } catch (e) {
    $("#ed-meta").textContent = "";
    toast("Couldn't open “" + item.name + "”: " + friendlyError(e, p.server), "error");
    closeEditor();
    return;
  }
  if (!editor.open || editor.item !== item) return; // closed while loading
  editor.original = data.content || "";
  ta.value = editor.original;
  ta.disabled = false;
  $("#ed-meta").textContent = humanSize(data.size || editor.original.length) + " · " + locationLabel(p.server, p.path);
  syncEditorView();
  setDirty(false);
  ta.focus();
  ta.setSelectionRange(0, 0);
}

function highlightEditor(text, lang) {
  const code = $("#ed-highlight code");
  // Highlighting escapes all file content; only fixed <span class="hl-*">
  // wrappers are introduced. A trailing newline keeps the layers aligned.
  if (text.length > 400000) code.textContent = text + "\n";
  else code.innerHTML = highlightCode(text, lang) + "\n";
}

const syncEditorViewSoon = debounce(() => syncEditorView(), 60);

// syncEditorView re-highlights and rebuilds the line-number gutter from the
// textarea's current content, and mirrors scroll between the layers.
function syncEditorView() {
  const ta = $("#ed-input");
  highlightEditor(ta.value, editor.lang);
  const lines = ta.value.split("\n").length;
  const gutter = $("#ed-gutter");
  if (gutter._lines !== lines) {
    let g = "";
    for (let k = 1; k <= lines; k++) g += k + "\n";
    gutter.textContent = g;
    gutter._lines = lines;
  }
  syncEditorScroll();
}

function syncEditorScroll() {
  const ta = $("#ed-input");
  const hl = $("#ed-highlight");
  hl.scrollTop = ta.scrollTop;
  hl.scrollLeft = ta.scrollLeft;
  $("#ed-gutter").scrollTop = ta.scrollTop;
}

function setDirty(d) {
  editor.dirty = d;
  $("#ed-dirty").hidden = !d;
}

function closeEditor() {
  $("#editor-overlay").hidden = true;
  editor.open = false;
  editor.item = null;
  const p = editor.p;
  editor.p = null;
  $("#ed-input").value = "";
  const back = editor.lastFocus;
  editor.lastFocus = null;
  if (back && back.isConnected) back.focus();
  else if (p && p.el) p.el.grid.focus();
}

async function saveEditor() {
  if (!editor.open) return;
  const p = editor.p;
  const item = editor.item;
  const ta = $("#ed-input");
  const content = ta.value;
  const full = fullPath(p, item);
  const saveBtn = $("#ed-save");
  saveBtn.disabled = true;
  saveBtn.textContent = "Saving…";
  try {
    const res = await fetch(api("/api/write", { server: p.server, path: full }), {
      method: "POST",
      headers: { "X-Fleet-Token": TOKEN, "Content-Type": "text/plain" },
      body: content,
    });
    if (!res.ok) throw await errorFrom(res);
  } catch (e) {
    toast("Save failed: " + friendlyError(e, p.server), "error");
    saveBtn.disabled = false;
    saveBtn.textContent = "Save";
    return;
  }
  editor.original = content;
  setDirty(false);
  saveBtn.disabled = false;
  saveBtn.textContent = "Save";
  toast("Saved “" + item.name + "”", "success");
  loadListing(p, { keepScroll: true, quiet: true });
}

function setupEditor() {
  const ta = $("#ed-input");
  ta.addEventListener("input", () => {
    if (ta.value.length > 200000) syncEditorViewSoon(); else syncEditorView();
    setDirty(ta.value !== editor.original);
  });
  ta.addEventListener("scroll", syncEditorScroll, { passive: true });
  $("#editor-overlay").addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") { e.preventDefault(); saveEditor(); return; }
    if (e.key === "Escape") { e.preventDefault(); tryCloseEditor(); return; }
    if (e.key === "Tab" && e.target === ta && !e.shiftKey) {
      // Tab inserts a soft tab; Shift+Tab (and Esc) leave the text area.
      e.preventDefault();
      const s = ta.selectionStart, en = ta.selectionEnd;
      ta.setRangeText("  ", s, en, "end");
      syncEditorView();
      setDirty(ta.value !== editor.original);
    }
  });
  $("#ed-save").addEventListener("click", saveEditor);
  $("#ed-cancel").addEventListener("click", tryCloseEditor);
}

function tryCloseEditor() {
  if (editor.dirty) {
    confirmDialog({
      title: "Discard unsaved changes?",
      message: "“" + (editor.item ? editor.item.name : "") + "” has changes that haven't been saved. Closing discards them.",
      okLabel: "Discard changes",
      danger: true,
    }).then((ok) => { if (ok) closeEditor(); else $("#ed-input").focus(); });
    return;
  }
  closeEditor();
}

// ----------------------------------------------------------------- startup

function showTakeover(title, lines) {
  const inner = h("div", { class: "to-inner" }, h("h2", { text: title }), lines.map((l) => h("p", null, l)));
  document.body.appendChild(h("div", { class: "takeover", role: "alert" }, inner));
}

// loadServers fetches the sources, restores the saved pane layout (when its
// sources still exist) or defaults to Local + the first reachable server.
async function loadServers() {
  let payload;
  try {
    payload = await getJSON("/api/servers");
  } catch (e) {
    toast("Couldn't load servers: " + friendlyError(e), "error");
    payload = { local: state.local, servers: [] };
  }
  // The object shape carries controller-local metadata separately from managed
  // targets. The array fallback keeps a graceful path for stale backends.
  state.local = Array.isArray(payload) ? state.local : payload.local || state.local;
  state.servers = Array.isArray(payload) ? payload : payload.servers || [];
  const known = (srv) => srv === "" || state.servers.some((s) => s.name === srv);
  const saved = store.get("layout", null);
  state.panes = [];
  if (saved && Array.isArray(saved.panes)) {
    for (const sp of saved.panes.slice(0, MAX_PANES)) {
      if (!sp || typeof sp.server !== "string" || !known(sp.server)) continue;
      const p = newPaneState(sp.server);
      applyPaneSource(p, sp.server, { path: typeof sp.path === "string" ? sp.path : "" });
      p.view = sp.view === "icons" ? "icons" : "list";
      p.hidden = !!sp.hidden;
      if (sp.sort && ["name", "size", "mod"].includes(sp.sort.key)) p.sort = { key: sp.sort.key, dir: sp.sort.dir < 0 ? -1 : 1 };
      state.panes.push(p);
    }
    state.active = Math.max(0, Math.min(Number(saved.active) || 0, state.panes.length - 1));
  }
  if (!state.panes.length) {
    const reachable = state.servers.filter((s) => s.reachable);
    const first = reachable[0] || state.servers[0];
    const a = newPaneState("");
    applyPaneSource(a, "");
    const b = newPaneState(first ? first.name : "");
    applyPaneSource(b, first ? first.name : "");
    state.panes.push(a, b);
    state.active = 0;
  }
  buildPanes();
  await Promise.all(state.panes.map((p) => loadListing(p)));
  setActivePane(activePane(), { focus: false });
}

function init() {
  if (!TOKEN) {
    showTakeover("Missing access token", [
      "Open the exact URL printed by `fleet file ui` (or `fleet fm ui`) — it carries this session's one-time access token.",
    ]);
    return;
  }
  $("#palette-kbd").textContent = MOD + " K";
  state.mobile = window.matchMedia("(max-width: 760px)").matches;
  document.body.classList.toggle("is-mobile", state.mobile);
  measureRowHeight();
  renderThemeToggle();

  setupModal();
  setupMenu();
  setupKeyboard();
  setupEditor();
  setupTransfers();
  setupPreview();
  setupOverview();

  $("#theme-toggle").addEventListener("click", toggleTheme);
  $("#open-palette").addEventListener("click", openPalette);
  $("#open-help").addEventListener("click", showShortcuts);
  for (const tab of $$(".view-tab")) tab.addEventListener("click", () => showView(tab.dataset.view));
  $(".views").addEventListener("keydown", (e) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    const next = state.view === "files" ? "overview" : "files";
    showView(next);
    $("#tab-" + next).focus();
  });

  const mq = (q, fn) => {
    const m = window.matchMedia(q);
    if (m.addEventListener) m.addEventListener("change", fn);
  };
  mq("(prefers-color-scheme: dark)", renderThemeToggle);
  mq("(max-width: 760px)", applyResponsive);
  mq("(pointer: coarse)", applyResponsive);
  window.addEventListener("resize", debounce(() => { for (const p of state.panes) renderRows(p); }, 60));
  window.addEventListener("blur", () => {
    if (drag.active) { drag.target = null; onDragUp({ clientX: drag.startX, clientY: drag.startY }); }
  });
  window.addEventListener("hashchange", () => {
    const v = location.hash === "#overview" ? "overview" : "files";
    if (v !== state.view) showView(v);
  });
  window.addEventListener("beforeunload", (e) => {
    const busy = xfer.list.some((t) => t.kind === "upload" && t.state === "running" && t.slot);
    if (busy || (editor.open && editor.dirty)) { e.preventDefault(); e.returnValue = ""; }
  });

  const initialView = location.hash === "#overview" ? "overview" : "files";
  state.view = initialView;
  loadServers().then(() => {
    showView(initialView);
    if (initialView !== "overview") loadOverview(); // fills the alert badge
  });
}

init();
