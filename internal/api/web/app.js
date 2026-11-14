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
const expanded = new Set();     // item IDs whose file list is open
const fileCache = new Map();    // item ID -> files
const rows = new Map();         // item ID -> <li>
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
    case "active": return t.phase === "metadata" ? "Finding peers" : "Downloading " + Math.floor((t.progress || 0) * 100) + "%";
    case "finalizing": return "Moving into the cache";
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
  $("stremio-result").hidden = true;
  addonURL = "";
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
    loadAddons();
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
    const body = await api("/v1/items");
    const list = (body && body.items) || [];
    $("banner").hidden = true;
    renderLibrary(list);
    $("lib-updated").textContent = "Updated " + new Date().toLocaleTimeString();
    if (list.some(inProgress)) next = FAST_POLL;
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

  const seen = new Set(list.map((t) => t.id));
  for (const [id, li] of rows) if (!seen.has(id)) { li.remove(); rows.delete(id); }

  if (list.length === 0) {
    const li = el("li", "empty");
    li.append(el("strong", "", "Your library is empty."), el("p", "", "Add a magnet link above. Torza downloads it once, then you can play it here."));
    ul.append(li);
    return;
  }

  list.forEach((t, i) => {
    let li = rows.get(t.id);
    if (!li) { li = buildRow(t.id); rows.set(t.id, li); }
    if (ul.children[i] !== li) ul.insertBefore(li, ul.children[i] || null);
    updateRow(li, t);
  });
}

function inProgress(t) { return t.status === "queued" || t.status === "active" || t.status === "finalizing"; }

function buildRow(id) {
  const li = el("li", "item");
  li.dataset.id = String(id);
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
  const hash = el("span", "hash", (t.hash || "").slice(0, 12) + "…");
  hash.title = t.hash || "";
  meta.replaceChildren(badge, " · " + size(t.size_bytes) + " · ", hash);

  // Progress bar while downloading.
  let bar = li.querySelector(".bar");
  if (inProgress(t)) {
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
      const toggle = el("button", "btn btn-secondary btn-sm", expanded.has(t.id) ? "Hide files" : "Show files");
      toggle.type = "button";
      toggle.setAttribute("aria-expanded", expanded.has(t.id) ? "true" : "false");
      toggle.addEventListener("click", () => toggleFiles(li, t.id, toggle));
      actions.append(toggle);
    }
    if (t.status === "evicted" || t.status === "failed") {
      const again = el("button", "btn btn-secondary btn-sm", "Download again");
      again.type = "button";
      again.addEventListener("click", () => readd(t.hash, t.id, again));
      actions.append(again);
    }
    const rm = el("button", "btn btn-danger btn-sm", "Remove");
    rm.type = "button";
    rm.setAttribute("aria-label", "Remove " + name + " from your library");
    rm.addEventListener("click", () => removeItem(t.id, t.hash, li));
    actions.append(rm);
  }

  // Files: shown while the item is ready and its list is open.
  if (t.status !== "ready") { li.querySelector(".files")?.remove(); }
  else if (expanded.has(t.id) && !li.querySelector(".files")) loadFiles(li, t.id);
}

async function toggleFiles(li, id, btn) {
  if (expanded.has(id)) {
    expanded.delete(id);
    li.querySelector(".files")?.remove();
    btn.textContent = "Show files"; btn.setAttribute("aria-expanded", "false");
    return;
  }
  expanded.add(id);
  btn.textContent = "Hide files"; btn.setAttribute("aria-expanded", "true");
  await loadFiles(li, id);
}

async function loadFiles(li, id) {
  let ul = li.querySelector(".files");
  if (!ul) { ul = el("ul", "files"); li.append(ul); }
  if (!fileCache.has(id)) {
    ul.replaceChildren(el("li", "file muted small", "Loading files…"));
    try {
      const t = await api("/v1/items/" + id);
      fileCache.set(id, (t && t.files) || []);
    } catch (e) {
      ul.replaceChildren(el("li", "file item-error", "Couldn't load the file list: " + e.message));
      return;
    }
  }
  const files = fileCache.get(id);
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
    const t = await api("/v1/items", { method: "POST", body: JSON.stringify({ magnet: value }) });
    $("magnet").value = "";
    clearTimeout(checkTimer); checkSeq++; setAvail("", "");
    const what = t.name || "the torrent";
    setAddMsg(t.status === "ready" ? "Added " + what + ". It was already cached, so it's ready now." : "Added " + what + ". Progress shows in your library below.", "ok");
    if (t.status === "ready") expanded.add(t.id);
    poll();
  } catch (e) {
    if (e.status === 400) setAddMsg("That isn't a magnet link or info hash. Paste one that starts with magnet:?xt=urn:btih:, or a 40-character hash.", "error");
    else if (e.status !== 401) setAddMsg(e.message, "error");
  } finally {
    btn.disabled = false; btn.textContent = "Add torrent";
  }
});

// ── Availability, like a debrid service's "instant" check ─────────────────

let checkTimer = null;
let checkSeq = 0;
const LOOKS_ADDABLE = /^(magnet:\?|[0-9a-f]{40}$|[a-z2-7]{32}$)/i;

function setAvail(text, kind) {
  const a = $("avail");
  a.textContent = text; a.hidden = !text;
  a.className = "field-msg avail" + (kind ? " is-" + kind : "");
}

$("magnet").addEventListener("input", () => {
  clearTimeout(checkTimer);
  const value = $("magnet").value.trim();
  if (!LOOKS_ADDABLE.test(value)) { setAvail("", ""); return; }
  checkTimer = setTimeout(() => checkAvailability(value), 350);
});

async function checkAvailability(value) {
  const seq = ++checkSeq;
  setAvail("Checking the cache…", "");
  try {
    const r = await api("/v1/items/check", { method: "POST", body: JSON.stringify({ items: [value] }) });
    if (seq !== checkSeq) return; // the field changed meanwhile
    const c = r.items[0];
    switch (c.availability) {
      case "cached": setAvail("Cached: ready instantly. " + (c.name || "") + " (" + size(c.size_bytes) + ", " + c.files.length + (c.files.length === 1 ? " file)" : " files)"), "ok"); break;
      case "downloading": setAvail("Someone is already downloading this. Adding it is free; it joins your library and becomes ready with theirs.", ""); break;
      case "not_cached": setAvail("Not cached yet. Adding it starts a download, which counts toward your active downloads.", ""); break;
      case "blocked": setAvail("This was removed after a takedown request and can't be added.", "error"); break;
      default: setAvail("", "");
    }
  } catch (e) {
    if (seq === checkSeq) setAvail("", "");
  }
}

async function readd(hash, id, btn) {
  btn.disabled = true; btn.textContent = "Adding…";
  try { await api("/v1/items", { method: "POST", body: JSON.stringify({ magnet: hash }) }); fileCache.delete(id); poll(); }
  catch (e) { if (e.status !== 401) showToast(e.message, null); btn.disabled = false; btn.textContent = "Download again"; }
}

async function removeItem(id, hash, li) {
  const name = li.querySelector(".item-name").textContent;
  li.hidden = true;
  try {
    await api("/v1/items/" + id, { method: "DELETE" });
    rows.delete(id); li.remove(); expanded.delete(id); fileCache.delete(id);
    showToast("Removed " + name + " from your library.", hash && (async () => {
      await api("/v1/items", { method: "POST", body: JSON.stringify({ magnet: hash }) });
      poll();
    }));
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

// ── Stremio add-on links ───────────────────────────────────────────────────

let addonURL = "";

async function loadAddons() {
  try {
    const r = await api("/v1/stremio");
    const ul = $("stremio-list");
    ul.replaceChildren(...(r.addons || []).map(addonRow));
  } catch (e) { /* the library banner already reports connection problems */ }
}

function addonRow(a) {
  const li = el("li", "addon");
  const label = a.label || "Unnamed device";
  const name = el("span", "addon-name", label);
  const used = el("span", "addon-used", a.last_used_at ? "Used " + new Date(a.last_used_at).toLocaleDateString() : "Not used yet");
  const rm = el("button", "btn btn-danger btn-sm", "Remove");
  rm.type = "button";
  rm.setAttribute("aria-label", "Remove the Stremio link for " + label);
  rm.addEventListener("click", async () => {
    rm.disabled = true; rm.textContent = "Removing…";
    try { await api("/v1/stremio/" + a.id, { method: "DELETE" }); li.remove(); showToast("Removed the Stremio link for " + label + ". Stremio on that device stops showing your library.", null); }
    catch (e) { rm.disabled = false; rm.textContent = "Remove"; if (e.status !== 401) showToast("Couldn't remove it: " + e.message, null); }
  });
  li.append(name, used, rm);
  return li;
}

$("stremio-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const btn = $("stremio-create");
  btn.disabled = true; btn.textContent = "Making link…";
  $("stremio-error").hidden = true;
  try {
    const r = await api("/v1/stremio", { method: "POST", body: JSON.stringify({ label: $("stremio-label").value.trim() }) });
    addonURL = r.manifest_url;
    $("stremio-install").href = r.install_url;
    $("stremio-copy-msg").textContent = "";
    $("stremio-result").hidden = false;
    $("stremio-url").value = addonURL;
    // A stremio:// link always opens over HTTPS, so on plain HTTP the URL has
    // to be pasted into Stremio. Stremio accepts plain-HTTP add-ons only
    // from 127.0.0.1 on the computer running Torza.
    const local = location.hostname === "127.0.0.1" || location.hostname === "localhost";
    const https = location.protocol === "https:";
    $("stremio-install").hidden = !https;
    if (https) {
      $("stremio-hint").textContent = "\"Install in Stremio\" works on the device you're using now. For a TV or phone, paste the URL into Stremio's add-on search box.";
    } else if (local) {
      $("stremio-hint").textContent = "In Stremio, open Add-ons (the puzzle icon), paste this URL into the search box at the top, press Enter, then click Install. Your library is then in Discover → Torza library. This works on this computer only; phones and TVs need Torza on HTTPS (the server deploy).";
    } else {
      $("stremio-hint").textContent = "Stremio won't install this: it accepts plain-HTTP add-ons only from 127.0.0.1. On the Mac running Torza, open http://127.0.0.1:" + (location.port || "80") + " and make the link there. Phones and TVs need Torza on HTTPS (the server deploy).";
    }
    $("stremio-label").value = "";
    ($("stremio-install").hidden ? $("stremio-copy") : $("stremio-install")).focus();
    loadAddons();
  } catch (e) {
    if (e.status !== 401) { $("stremio-error").textContent = "Couldn't make a link: " + e.message; $("stremio-error").hidden = false; }
  } finally {
    btn.disabled = false; btn.textContent = "Make add-on link";
  }
});

$("stremio-copy").addEventListener("click", async () => {
  try { await navigator.clipboard.writeText(addonURL); $("stremio-copy-msg").textContent = "Copied."; }
  catch { $("stremio-copy-msg").textContent = "Copy failed. Select it here: " + addonURL; }
});

// ── Start ──────────────────────────────────────────────────────────────────

if (apiKey) connect(); else showGate("");
