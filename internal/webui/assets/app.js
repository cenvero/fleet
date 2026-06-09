// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh
//
// Self-contained front-end for the Cenvero Fleet web file manager. Talks to the
// localhost controller over the token-gated /api endpoints. No external deps.

const TOKEN = new URLSearchParams(location.search).get("t") || "";

// Drop the token from the address bar/history once captured, so it doesn't
// linger in browser history or get copy-pasted from the URL bar.
if (TOKEN && window.history && window.history.replaceState) {
  window.history.replaceState(null, "", location.pathname);
}

const state = {
  server: "",
  path: "/",
};

const el = (id) => document.getElementById(id);

function api(pathname, params = {}) {
  const url = new URL(pathname, location.origin);
  url.searchParams.set("t", TOKEN);
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
  return url;
}

async function getJSON(pathname, params) {
  const res = await fetch(api(pathname, params), { headers: { "X-Fleet-Token": TOKEN } });
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.json();
}

// postJSON is used for state-changing endpoints (mkdir/rm/mv), which are
// POST-only and Origin-checked on the server.
async function postJSON(pathname, params) {
  const res = await fetch(api(pathname, params), { method: "POST", headers: { "X-Fleet-Token": TOKEN } });
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.json();
}

function toast(message, isError = false) {
  const t = el("toast");
  t.textContent = message;
  t.className = "toast" + (isError ? " error" : "");
  t.hidden = false;
  clearTimeout(toast._timer);
  toast._timer = setTimeout(() => (t.hidden = true), 3500);
}

function humanSize(n) {
  if (n < 1024) return n + " B";
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(1) + " " + units[i];
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

async function loadServers() {
  let servers = [];
  try {
    servers = await getJSON("/api/servers");
  } catch (e) {
    toast("Failed to load servers: " + e.message, true);
    return;
  }
  const sel = el("server");
  sel.innerHTML = "";
  for (const s of servers) {
    const opt = document.createElement("option");
    opt.value = s.name;
    opt.textContent = s.reachable ? s.name : s.name + " (offline)";
    sel.appendChild(opt);
  }
  if (servers.length) {
    state.server = servers[0].name;
    await loadListing();
  } else {
    el("entries").innerHTML = '<p class="muted empty">No servers. Add one with `fleet server add`.</p>';
  }
}

async function loadListing() {
  if (!state.server) return;
  const entries = el("entries");
  entries.innerHTML = '<p class="muted empty">Loading…</p>';
  let result;
  try {
    result = await getJSON("/api/list", { server: state.server, path: state.path });
  } catch (e) {
    // textContent (not innerHTML) — backend error text is untrusted.
    const p = document.createElement("p");
    p.className = "muted empty";
    p.textContent = "Error: " + e.message;
    entries.replaceChildren(p);
    return;
  }
  state.path = result.path || state.path;
  el("breadcrumb").textContent = state.path;
  renderEntries(result.entries || []);
}

function renderEntries(items) {
  const container = el("entries");
  container.innerHTML = "";
  if (!items.length) {
    container.innerHTML = '<p class="muted empty">(empty)</p>';
    return;
  }
  for (const item of items) {
    const row = document.createElement("div");
    row.className = "row" + (item.is_dir ? " dir" : "");

    const icon = document.createElement("span");
    icon.className = "icon";
    icon.textContent = item.is_dir ? "▸" : "·";

    const name = document.createElement("span");
    name.className = "name";
    name.textContent = item.name;
    if (item.is_dir) {
      name.onclick = () => { state.path = joinPath(state.path, item.name); loadListing(); };
    }

    const size = document.createElement("span");
    size.className = "size";
    size.textContent = item.is_dir ? "" : humanSize(item.size);

    const actions = document.createElement("span");
    actions.className = "actions";
    if (!item.is_dir) {
      const dl = document.createElement("button");
      dl.textContent = "download";
      dl.onclick = () => downloadFile(item.name);
      actions.appendChild(dl);
    }
    const rm = document.createElement("button");
    rm.textContent = "delete";
    rm.onclick = () => removeEntry(item);
    actions.appendChild(rm);

    row.append(icon, name, size, actions);
    container.appendChild(row);
  }
}

function downloadFile(name) {
  const url = api("/api/download", { server: state.server, path: joinPath(state.path, name) });
  window.location.assign(url.toString());
}

async function removeEntry(item) {
  if (!confirm(`Delete ${item.name}?`)) return;
  try {
    await postJSON("/api/rm", {
      server: state.server,
      path: joinPath(state.path, item.name),
      recursive: item.is_dir ? "true" : "false",
    });
    toast("Deleted " + item.name);
    loadListing();
  } catch (e) {
    toast("Delete failed: " + e.message, true);
  }
}

async function makeDir() {
  const name = prompt("New folder name:");
  if (!name) return;
  try {
    await postJSON("/api/mkdir", { server: state.server, path: joinPath(state.path, name) });
    toast("Created " + name);
    loadListing();
  } catch (e) {
    toast("mkdir failed: " + e.message, true);
  }
}

// ---- uploads ----

async function uploadFiles(fileList) {
  for (const file of fileList) {
    const url = api("/api/upload", { server: state.server, dir: state.path, name: file.name });
    let resp;
    try {
      const res = await fetch(url, {
        method: "POST",
        headers: { "X-Fleet-Token": TOKEN },
        body: file,
      });
      if (!res.ok) throw new Error((await res.text()) || res.statusText);
      resp = await res.json();
    } catch (e) {
      toast("Upload failed: " + e.message, true);
      continue;
    }
    watchTransfer(resp.id, "↑ " + file.name, file.size);
  }
  loadListing();
}

const transfers = new Map();

function watchTransfer(id, label, total) {
  const card = document.createElement("div");
  card.className = "transfer";
  card.innerHTML = `
    <div class="label"><span class="name"></span><span class="pct">0%</span></div>
    <div class="progress"><span></span></div>
    <div class="meta">starting…</div>`;
  card.querySelector(".name").textContent = label;
  const list = el("transfers");
  const placeholder = list.querySelector(".empty");
  if (placeholder) placeholder.remove();
  list.prepend(card);
  transfers.set(id, card);

  const es = new EventSource(api("/api/progress", { id }).toString());
  es.onmessage = (ev) => {
    const p = JSON.parse(ev.data);
    card.querySelector(".progress > span").style.width = p.percent + "%";
    card.querySelector(".pct").textContent = p.percent + "%";
    if (p.error) {
      card.classList.add("error");
      card.querySelector(".meta").textContent = "error: " + p.error;
    } else if (p.done) {
      card.classList.add("done");
      card.querySelector(".meta").textContent = "done · " + humanSize(p.total_bytes);
    } else {
      const rate = p.rate_per_sec > 0 ? humanSize(p.rate_per_sec) + "/s" : "…";
      card.querySelector(".meta").textContent =
        `${humanSize(p.bytes_done)} / ${humanSize(p.total_bytes)} · ${rate} · ${p.active_streams} streams`;
    }
    if (p.done) { es.close(); loadListing(); }
  };
  es.onerror = () => es.close();
}

// ---- wiring ----

function setupDropzone() {
  const zone = el("dropzone");
  const stop = (e) => { e.preventDefault(); e.stopPropagation(); };
  ["dragenter", "dragover"].forEach((evt) =>
    zone.addEventListener(evt, (e) => { stop(e); zone.classList.add("dragover"); })
  );
  ["dragleave", "drop"].forEach((evt) =>
    zone.addEventListener(evt, (e) => { stop(e); if (evt === "drop" || e.target === zone) zone.classList.remove("dragover"); })
  );
  zone.addEventListener("drop", (e) => {
    if (e.dataTransfer && e.dataTransfer.files.length) uploadFiles(e.dataTransfer.files);
  });
}

function init() {
  if (!TOKEN) {
    document.body.innerHTML = '<p style="padding:40px;color:#ff6b6b">Missing access token. Open the URL printed by `fleet ui`.</p>';
    return;
  }
  el("server").onchange = (e) => { state.server = e.target.value; state.path = "/"; loadListing(); };
  el("refresh").onclick = () => loadListing();
  el("up").onclick = () => { state.path = parentPath(state.path); loadListing(); };
  el("mkdir").onclick = makeDir;
  el("pick").onclick = () => el("file-input").click();
  el("file-input").onchange = (e) => { if (e.target.files.length) uploadFiles(e.target.files); e.target.value = ""; };
  setupDropzone();
  loadServers();
}

init();
