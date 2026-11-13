// Torza test page. Talks to the REST API on this origin with the API key
// saved in this browser, and plays files through signed links from the
// stream service. Torrent and file names come from torrent metadata, which
// anyone can write, so they are only ever inserted as text (textContent).
"use strict";

const KEY_STORE = "torza.apiKey";
const FAST_POLL = 3000;   // while something is queued or downloading
const SLOW_POLL = 15000;  // otherwise
const RETRY_POLL = 5000;  // after the API couldn't be reached
const VIDEO = /\.(mp4|m4v|mkv|webm|mov|avi|ts|m2ts)$/i;

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
};

let apiKey = readKey();
let pollTimer = null;
let firstLoad = true;
const expanded = new Set();     // infohashes whose file list is open
const fileCache = new Map();    // infohash -> files
const rows = new Map();         // infohash -> <li>
let playing = { fileId: null, url: null };
let undo = null;                // pending undo for the last removal

function readKey() { try { return localStorage.getItem(KEY_STORE) || ""; } catch { return ""; } }
function saveKey(k) { try { k ? localStorage.setItem(KEY_STORE, k) : localStorage.removeItem(KEY_STORE); } catch { /* private mode: key lives for this tab only */ } }

class APIError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(path, opts = {}) {
  let res;
  try {
    res = await fetch(path, {
      ...opts,
      headers: { "Authorization": "Bearer " + apiKey, ...(opts.body ? { "Content-Type": "application/json" } : {}) },
    });
  } catch {
    throw new APIError(0, "Can't reach Torza at " + location.host + ". Is it running? Start it with deploy/local.sh up.");
  }
  let body = null;
  if (res.status !== 204) { try { body = await res.json(); } catch { body = null; } }
  if (!res.ok) {
    const msg = (body && body.error) || ("Torza answered with HTTP " + res.status + ".");
    if (res.status === 401) showGate("That API key isn't valid. Paste the one from deploy/.local-api-key.");
    throw new APIError(res.status, msg);
  }
  return body;
}

// ── Formatting ─────────────────────────────────────────────────────────────

function size(bytes) {
  if (!bytes) return "size not known yet";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, n = bytes;
  while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 2 : 1)) + " " + units[i];
}

function statusLabel(t) {
  switch (t.status) {
    case "queued": return "Queued";
    case "downloading": return "Downloading " + Math.floor((t.progress || 0) * 100) + "%";
    case "ready": return "Ready";
    case "failed": return "Failed";
    case "evicted": return "Removed from cache";
    case "blocked": return "Blocked";
    default: return t.status;
  }
}

function baseName(path) { const parts = path.split("/"); return parts[parts.length - 1]; }

// ── Gate (API key) ─────────────────────────────────────────────────────────

function showGate(error) {
  stopPolling();
  saveKey("");
  apiKey = "";
  $("app").hidden = true;
  $("who").hidden = true;
  $("change-key").hidden = true;
  $("banner").hidden = true;
  $("gate").hidden = false;
  const err = $("key-error");
  err.hidden = !error;
  err.textContent = error || "";
  $("key").setAttribute("aria-invalid", error ? "true" : "false");
  if (error) $("key").focus();
}

async function connect() {
  try {
    const me = await api("/v1/me");
    $("gate").hidden = true;
    $("app").hidden = false;
    $("who").textContent = me.email + (me.is_admin ? " (admin)" : "");
    $("who").hidden = false;
    $("change-key").hidden = false;
    renderSkeleton();
    poll();
    return true;
  } catch (e) {
    if (e.status === 401) return false; // api() already showed the gate
    showGate(e.message);
    return false;
  }
}

$("key-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const k = $("key").value.trim();
  if (!k) {
    $("key-error").textContent = "Paste your API key first.";
    $("key-error").hidden = false;
    $("key").setAttribute("aria-invalid", "true");
    return;
  }
  const btn = $("key-submit");
  btn.disabled = true; btn.textContent = "Connecting…";
  apiKey = k;
  if (await connect()) { saveKey(k); $("key").value = ""; }
  btn.disabled = false; btn.textContent = "Connect";
});

$("change-key").addEventListener("click", () => showGate(""));

// ── Library ────────────────────────────────────────────────────────────────

function stopPolling() { clearTimeout(pollTimer); pollTimer = null; }

async function poll() {
  stopPolling();
  let next = SLOW_POLL;
  try {
    const body = await api("/v1/torrents");
    const list = (body && body.torrents) || [];
    $("banner").hidden = true;
    renderLibrary(list);
    $("lib-updated").textContent = "Updated " + new Date().toLocaleTimeString();
    if (list.some((t) => t.status === "queued" || t.status === "downloading")) next = FAST_POLL;
  } catch (e) {
    if (e.status === 401) return;
    if (!(e instanceof APIError)) console.error(e);
    $("banner").textContent = e instanceof APIError
      ? e.message + " Showing what was loaded last; retrying."
      : "This page hit a bug (" + e.message + "). Reload it; if it keeps happening, check the browser console.";
    $("banner").hidden = false;
    next = RETRY_POLL;
  }
  if (apiKey) pollTimer = setTimeout(poll, next);
}

function renderSkeleton() {
  const ul = $("library");
  ul.replaceChildren();
  rows.clear();
  for (let i = 0; i < 3; i++) { const li = el("li", "skeleton"); li.setAttribute("aria-hidden", "true"); ul.append(li); }
  ul.setAttribute("aria-busy", "true");
  firstLoad = true;
}

function renderLibrary(list) {
  const ul = $("library");
  if (firstLoad) { ul.replaceChildren(); firstLoad = false; }
  ul.setAttribute("aria-busy", "false");
  ul.querySelector(".empty")?.remove();

  const seen = new Set(list.map((t) => t.infohash));
  for (const [hash, li] of rows) if (!seen.has(hash)) { li.remove(); rows.delete(hash); }

  if (list.length === 0) {
    const li = el("li", "empty");
    li.append(el("strong", "", "Your library is empty."), el("p", "", "Add a magnet link above. Torza downloads it once, then you can play it here."));
    ul.append(li);
    return;
  }

  list.forEach((t, i) => {
    let li = rows.get(t.infohash);
    if (!li) { li = buildRow(t.infohash); rows.set(t.infohash, li); }
    if (ul.children[i] !== li) ul.insertBefore(li, ul.children[i] || null);
    updateRow(li, t);
  });
}

function buildRow(hash) {
  const li = el("li", "item");
  li.dataset.hash = hash;
  const head = el("div", "item-head");
  const main = el("div", "item-main");
  main.append(el("p", "item-name"), el("p", "item-meta"));
  const actions = el("div", "item-actions");
  head.append(main, actions);
  li.append(head);
  return li;
}

function updateRow(li, t) {
  const name = t.name || "Waiting for the torrent's name…";
  li.querySelector(".item-name").textContent = name;

  const meta = li.querySelector(".item-meta");
  const badge = el("span", "status " + t.status, statusLabel(t));
  const hash = el("span", "hash", t.infohash.slice(0, 12) + "…");
  hash.title = t.infohash;
  meta.replaceChildren(badge, " · " + size(t.size_bytes) + " · ", hash);

  // Progress bar while downloading.
  let bar = li.querySelector(".bar");
  if (t.status === "downloading" || t.status === "queued") {
    if (!bar) { bar = el("progress", "bar"); bar.max = 100; li.querySelector(".item-main").append(bar); }
    bar.value = Math.floor((t.progress || 0) * 100);
    bar.setAttribute("aria-label", "Download progress for " + name);
  } else if (bar) bar.remove();

  // Error text for failed items.
  let err = li.querySelector(".item-error");
  if (t.error && t.status === "failed") {
    if (!err) { err = el("p", "item-error"); li.querySelector(".item-main").append(err); }
    err.textContent = t.error;
  } else if (err) err.remove();

  // Actions: rebuilt only when the status changes, so focus isn't lost on every poll.
  const actions = li.querySelector(".item-actions");
  if (actions.dataset.status !== t.status) {
    actions.dataset.status = t.status;
    actions.replaceChildren();
    if (t.status === "ready") {
      const toggle = el("button", "btn btn-secondary btn-sm", expanded.has(t.infohash) ? "Hide files" : "Show files");
      toggle.type = "button";
      toggle.setAttribute("aria-expanded", expanded.has(t.infohash) ? "true" : "false");
      toggle.addEventListener("click", () => toggleFiles(li, t.infohash, toggle));
      actions.append(toggle);
    }
    if (t.status === "evicted" || t.status === "failed") {
      const again = el("button", "btn btn-secondary btn-sm", "Download again");
      again.type = "button";
      again.addEventListener("click", () => readd(t.infohash, again));
      actions.append(again);
    }
    const rm = el("button", "btn btn-danger btn-sm", "Remove");
    rm.type = "button";
    rm.setAttribute("aria-label", "Remove " + name + " from your library");
    rm.addEventListener("click", () => removeItem(t.infohash, li));
    actions.append(rm);
  }

  // Files: shown while the item is ready and its list is open.
  if (t.status !== "ready") { li.querySelector(".files")?.remove(); }
  else if (expanded.has(t.infohash) && !li.querySelector(".files")) loadFiles(li, t.infohash);
}

async function toggleFiles(li, hash, btn) {
  if (expanded.has(hash)) {
    expanded.delete(hash);
    li.querySelector(".files")?.remove();
    btn.textContent = "Show files"; btn.setAttribute("aria-expanded", "false");
    return;
  }
  expanded.add(hash);
  btn.textContent = "Hide files"; btn.setAttribute("aria-expanded", "true");
  await loadFiles(li, hash);
}

async function loadFiles(li, hash) {
  let ul = li.querySelector(".files");
  if (!ul) { ul = el("ul", "files"); li.append(ul); }
  if (!fileCache.has(hash)) {
    ul.replaceChildren(el("li", "file muted small", "Loading files…"));
    try {
      const t = await api("/v1/torrents/" + encodeURIComponent(hash));
      fileCache.set(hash, (t && t.files) || []);
    } catch (e) {
      ul.replaceChildren(el("li", "file item-error", "Couldn't load the file list: " + e.message));
      return;
    }
  }
  const files = fileCache.get(hash);
  if (!files.length) { ul.replaceChildren(el("li", "file muted small", "This torrent has no files.")); return; }
  const sorted = [...files].sort((a, b) => (VIDEO.test(b.path) - VIDEO.test(a.path)) || (b.size_bytes - a.size_bytes));
  ul.replaceChildren(...sorted.map(fileRow));
}

function fileRow(f) {
  const li = el("li", "file" + (playing.fileId === f.id ? " playing" : ""));
  li.dataset.fileId = String(f.id);
  const name = el("span", "file-name", baseName(f.path));
  name.title = f.path;
  li.append(name, el("span", "file-size", size(f.size_bytes)));
  if (VIDEO.test(f.path)) {
    const play = el("button", "btn btn-primary btn-sm", "Play");
    play.type = "button";
    play.setAttribute("aria-label", "Play " + baseName(f.path));
    play.addEventListener("click", () => playFile(f, play));
    li.append(play);
  }
  const copy = el("button", "btn btn-plain btn-sm", "Copy link");
  copy.type = "button";
  copy.setAttribute("aria-label", "Copy link to " + baseName(f.path));
  copy.addEventListener("click", () => copyFileLink(f, copy));
  li.append(copy);
  return li;
}

// ── Adding, removing ───────────────────────────────────────────────────────

function setAddMsg(text, kind) {
  const m = $("add-msg");
  m.textContent = text; m.hidden = !text;
  m.className = "field-msg" + (kind ? " is-" + kind : "");
  $("magnet").setAttribute("aria-invalid", kind === "error" ? "true" : "false");
}

$("add-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const value = $("magnet").value.trim();
  if (!value) { setAddMsg("Paste a magnet link or a 40-character info hash.", "error"); $("magnet").focus(); return; }
  const btn = $("add-submit");
  btn.disabled = true; btn.textContent = "Adding…";
  setAddMsg("", "");
  try {
    const t = await api("/v1/torrents", { method: "POST", body: JSON.stringify({ magnet: value }) });
    $("magnet").value = "";
    const what = t.name || "the torrent";
    setAddMsg(t.status === "ready" ? "Added " + what + ". It was already cached, so it's ready now." : "Added " + what + ". Progress shows in your library below.", "ok");
    if (t.status === "ready") expanded.add(t.infohash);
    poll();
  } catch (e) {
    if (e.status === 400) setAddMsg("That isn't a magnet link or info hash. Paste one that starts with magnet:?xt=urn:btih:, or a 40-character hash.", "error");
    else if (e.status !== 401) setAddMsg(e.message, "error");
  } finally {
    btn.disabled = false; btn.textContent = "Add torrent";
  }
});

async function readd(hash, btn) {
  btn.disabled = true; btn.textContent = "Adding…";
  try { await api("/v1/torrents", { method: "POST", body: JSON.stringify({ magnet: hash }) }); fileCache.delete(hash); poll(); }
  catch (e) { if (e.status !== 401) showToast(e.message, null); btn.disabled = false; btn.textContent = "Download again"; }
}

async function removeItem(hash, li) {
  const name = li.querySelector(".item-name").textContent;
  li.hidden = true;
  try {
    await api("/v1/torrents/" + encodeURIComponent(hash), { method: "DELETE" });
    rows.delete(hash); li.remove(); expanded.delete(hash); fileCache.delete(hash);
    showToast("Removed " + name + " from your library.", async () => {
      await api("/v1/torrents", { method: "POST", body: JSON.stringify({ magnet: hash }) });
      poll();
    });
    poll();
  } catch (e) {
    li.hidden = false;
    if (e.status !== 401) showToast("Couldn't remove " + name + ": " + e.message, null);
  }
}

// ── Player and links ───────────────────────────────────────────────────────

async function linkFor(fileId) {
  const r = await api("/v1/files/" + fileId + "/link", { method: "POST" });
  return r.url;
}

async function playFile(f, btn) {
  btn.disabled = true; btn.textContent = "Loading…";
  $("player-error").hidden = true;
  try {
    const url = await linkFor(f.id);
    playing = { fileId: f.id, url };
    document.querySelectorAll(".file.playing").forEach((n) => n.classList.remove("playing"));
    document.querySelectorAll('.file[data-file-id="' + f.id + '"]').forEach((n) => n.classList.add("playing"));
    $("player-empty").hidden = true;
    $("player-body").hidden = false;
    $("now-playing").textContent = baseName(f.path);
    $("copy-msg").textContent = "";
    const v = $("video");
    v.src = url;
    v.play().catch(() => { /* autoplay can be refused; the controls still work */ });
    if (window.matchMedia("(max-width: 999px)").matches) $("player").scrollIntoView({ behavior: "smooth", block: "start" });
  } catch (e) {
    if (e.status !== 401) showToast("Couldn't start " + baseName(f.path) + ": " + e.message, null);
  } finally {
    btn.disabled = false; btn.textContent = "Play";
  }
}

$("video").addEventListener("error", () => {
  if (!$("video").getAttribute("src")) return;
  const e = $("player-error");
  e.textContent = "This browser can't play this file. Its format or codec isn't supported here. Copy the link and open it in VLC.";
  e.hidden = false;
});

async function copy(text) {
  try { await navigator.clipboard.writeText(text); return true; } catch { return false; }
}

$("copy-link").addEventListener("click", async () => {
  if (!playing.url) return;
  $("copy-msg").textContent = (await copy(playing.url)) ? "Copied. In VLC: File → Open Network." : "Couldn't copy. Your browser blocked the clipboard.";
});

async function copyFileLink(f, btn) {
  btn.disabled = true;
  try {
    const url = await linkFor(f.id);
    showToast((await copy(url)) ? "Link to " + baseName(f.path) + " copied. It works for 6 hours." : "Couldn't copy. Your browser blocked the clipboard.", null);
  } catch (e) {
    if (e.status !== 401) showToast("Couldn't get a link: " + e.message, null);
  } finally { btn.disabled = false; }
}

// ── Toast ──────────────────────────────────────────────────────────────────

let toastTimer = null;
function showToast(text, onUndo) {
  clearTimeout(toastTimer);
  undo = onUndo;
  $("toast-text").textContent = text;
  $("toast-undo").hidden = !onUndo;
  $("toast").hidden = false;
  toastTimer = setTimeout(() => { $("toast").hidden = true; undo = null; }, onUndo ? 8000 : 5000);
}
$("toast-undo").addEventListener("click", async () => {
  const fn = undo; undo = null; $("toast").hidden = true;
  if (!fn) return;
  try { await fn(); showToast("Added back to your library.", null); }
  catch (e) { if (e.status !== 401) showToast("Couldn't add it back: " + e.message, null); }
});
document.addEventListener("keydown", (ev) => { if (ev.key === "Escape" && !$("toast").hidden) { $("toast").hidden = true; undo = null; } });

// ── Start ──────────────────────────────────────────────────────────────────

if (apiKey) connect(); else showGate("");
