/* yap client.
 *
 * No framework and no build step: the whole UI is one file the binary serves.
 * State arrives two ways — a full snapshot from /api/state, and a stream of
 * deltas over server-sent events — and the render is deliberately dumb, so a
 * message that arrives while you are scrolled up cannot desync the view.
 */

const $ = (id) => document.getElementById(id);

const el = {
  app: $("app"),
  myName: $("my-name"),
  myAddress: $("my-address"),
  myAddressText: $("my-address-text"),
  themeToggle: $("theme-toggle"),
  gPeers: $("g-peers"), gLinks: $("g-links"),
  gPending: $("g-pending"), gCarrying: $("g-carrying"),
  airspace: $("airspace"),
  chats: $("chats"),
  addContact: $("add-contact"),
  blank: $("blank"),
  head: $("thread-head"), headTitle: $("head-title"),
  headSub: $("head-sub"), headRange: $("head-range"),
  back: $("back"),
  messages: $("messages"),
  composer: $("composer"), input: $("input"), send: $("send"),
  file: $("file"), attach: $("attach"),
  attachPreview: $("attach-preview"), attachThumb: $("attach-thumb"),
  attachName: $("attach-name"), attachClear: $("attach-clear"),
  sheet: $("sheet"), addAddress: $("add-address"), addName: $("add-name"),
  addError: $("add-error"), addSave: $("add-save"),
  toast: $("toast"),
  lightbox: $("lightbox"), lightboxImg: $("lightbox-img"),
};

const state = {
  me: null,
  chats: [],
  contacts: [],
  peers: {},          // node id -> hops
  open: null,         // node id of the open conversation
  messages: [],
  typingUntil: 0,
  attachment: null,
};

/* ---------------------------------------------------------------- helpers */

async function api(path, body) {
  const res = await fetch(path, body
    ? { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }
    : undefined);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `request failed (${res.status})`);
  return data;
}

function toast(msg) {
  el.toast.textContent = msg;
  el.toast.classList.add("on");
  clearTimeout(toast.t);
  toast.t = setTimeout(() => el.toast.classList.remove("on"), 2200);
}

function clock(ms) {
  return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function dayLabel(ms) {
  const d = new Date(ms), now = new Date();
  const sameDay = (a, b) => a.toDateString() === b.toDateString();
  if (sameDay(d, now)) return "Today";
  const yest = new Date(now); yest.setDate(now.getDate() - 1);
  if (sameDay(d, yest)) return "Yesterday";
  return d.toLocaleDateString([], { weekday: "short", day: "numeric", month: "short" });
}

// Relative time for the chat list, where an exact clock is noise.
function ago(ms) {
  if (!ms) return "";
  const s = (Date.now() - ms) / 1000;
  if (s < 60) return "now";
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  if (s < 604800) return `${Math.floor(s / 86400)}d`;
  return new Date(ms).toLocaleDateString([], { day: "numeric", month: "short" });
}

// Bars filled by proximity. This is the app's core spatial idea, so it gets
// one function every surface reads from.
function rangeLevel(nodeID) {
  const hops = state.peers[nodeID];
  if (hops === undefined) return 0;
  if (hops <= 0) return 3;
  if (hops === 1) return 2;
  return 1;
}

function rangeWords(nodeID) {
  const hops = state.peers[nodeID];
  if (hops === undefined) return "not in range";
  if (hops <= 0) return "nearby";
  return hops === 1 ? "1 hop away" : `${hops} hops away`;
}

function nameFor(nodeID) {
  const c = state.contacts.find((c) => c.node_id === nodeID);
  if (c) return c.name || c.address.replace(/^yap:/, "").slice(0, 11) + "…";
  return nodeID.slice(0, 10) + "…";
}

/* ------------------------------------------------------------------ ticks */

const TICK_ONE = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M4 12.5l5 5L20 6.5"/></svg>`;
const TICK_TWO = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12.5l5 5L17 6.5"/><path d="M8.5 15.5l2 2L21.5 6.5"/></svg>`;
const TICK_WAIT = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="12" cy="12" r="8.4"/><path d="M12 7.6V12l2.8 2"/></svg>`;
const TICK_FAIL = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="12" cy="12" r="8.4"/><path d="M12 7.8v5M12 16.1v.1"/></svg>`;

function tick(state_) {
  const label = {
    queued: "waiting to go out",
    sent: "sent",
    delivered: "delivered",
    read: "read",
    failed: "could not be delivered",
  }[state_] || state_;
  const glyph = state_ === "queued" ? TICK_WAIT
    : state_ === "failed" ? TICK_FAIL
    : state_ === "sent" ? TICK_ONE
    : TICK_TWO;
  return `<span class="tick" data-state="${state_}" title="${label}">${glyph}</span>`;
}

const FILE_ICON = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M14 3v5h5"/><path d="M14 3H6.5A1.5 1.5 0 0 0 5 4.5v15A1.5 1.5 0 0 0 6.5 21h11a1.5 1.5 0 0 0 1.5-1.5V8z"/></svg>`;

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

/* ----------------------------------------------------------------- render */

function renderMe() {
  if (!state.me) return;
  el.myAddressText.textContent = state.me.address;
  if (document.activeElement !== el.myName) el.myName.value = state.me.name || "";
}

function renderAirspace(s) {
  const set = (node, n) => {
    node.textContent = n;
    node.parentElement.classList.toggle("live", n > 0);
  };
  set(el.gPeers, s.peers ?? 0);
  set(el.gLinks, s.links ?? 0);
  el.gPending.textContent = s.pending ?? 0;
  el.gPending.parentElement.classList.toggle("wait", (s.pending ?? 0) > 0);
  set(el.gCarrying, s.carrying ?? 0);
}

function renderChats() {
  if (!state.chats.length) {
    el.chats.innerHTML = `<div class="rail-empty">
      <b>Nobody yet</b>
      Add someone by their address, or wait for a node nearby to say hello.
    </div>`;
    return;
  }

  el.chats.innerHTML = state.chats.map((c) => {
    const level = rangeLevel(c.id);
    const unread = c.unread > 0 ? `<span class="badge">${c.unread > 99 ? "99+" : c.unread}</span>` : "";
    return `<button class="chat" data-chat="${esc(c.id)}" aria-current="${c.id === state.open}">
      <span class="range" data-level="${level}" title="${rangeWords(c.id)}"><i></i><i></i><i></i></span>
      <span class="chat-body">
        <span class="chat-title">${esc(c.title || nameFor(c.id))}</span>
        <span class="chat-preview">${esc(c.preview || "No messages yet")}</span>
      </span>
      <span class="chat-meta">
        <span class="chat-time">${ago(c.last_at)}</span>
        ${unread}
      </span>
    </button>`;
  }).join("");
}

function renderThread() {
  const open = !!state.open;
  el.blank.hidden = open;
  el.head.hidden = !open;
  el.messages.hidden = !open;
  el.composer.hidden = !open;
  if (!open) return;

  const level = rangeLevel(state.open);
  el.headTitle.textContent = nameFor(state.open);
  el.headRange.dataset.level = level;
  const words = rangeWords(state.open);
  el.headSub.innerHTML = level
    ? `<span class="near">${esc(words)}</span> · end-to-end encrypted`
    : `${esc(words)} · messages wait until they are`;

  const typing = Date.now() < state.typingUntil;

  let lastDay = "", lastAuthor = null;
  const rows = state.messages.map((m) => {
    let out = "";
    const day = dayLabel(m.created_at);
    if (day !== lastDay) {
      out += `<div class="daybreak">${esc(day)}</div>`;
      lastDay = day;
      lastAuthor = null;
    }
    const run = lastAuthor === m.mine;
    lastAuthor = m.mine;

    let content = "";
    if (m.deleted) {
      content = `<div class="msg-text">This message was deleted</div>`;
    } else {
      if (m.blob_id && m.kind === "image") {
        content += `<img src="/blob/${esc(m.blob_id)}" alt="" loading="lazy">`;
      } else if (m.blob_id) {
        content += `<a class="msg-file" href="/blob/${esc(m.blob_id)}" download>${FILE_ICON}<span>attachment</span></a>`;
      }
      if (m.body) content += `<div class="msg-text">${esc(m.body)}</div>`;
    }

    const actions = m.mine && !m.deleted
      ? `<span class="msg-actions"><button data-del="${esc(m.id)}">delete</button></span>`
      : "";

    return out + `<div class="msg ${m.mine ? "mine" : ""} ${run ? "run" : ""} ${m.deleted ? "gone" : ""}">
      ${actions}${content}
      <div class="msg-foot">
        <span>${clock(m.created_at)}</span>
        ${m.mine ? tick(m.state) : ""}
      </div>
    </div>`;
  }).join("");

  const wasNearBottom = el.messages.scrollHeight - el.messages.scrollTop - el.messages.clientHeight < 120;
  el.messages.innerHTML = rows + (typing
    ? `<div class="typing"><i></i><i></i><i></i></div>` : "");
  if (wasNearBottom) el.messages.scrollTop = el.messages.scrollHeight;
}

/* ------------------------------------------------------------------ loads */

async function loadState() {
  const s = await api("/api/state");
  state.me = s.me;
  state.chats = s.chats || [];
  state.contacts = s.contacts || [];
  state.peers = s.nearby || {};
  renderMe();
  renderAirspace(s);
  renderChats();
  renderThread();
}

// Presence lives in the mesh rather than the database, and it moves whenever
// somebody walks. Poll it on its own cadence so the range meters stay honest
// without waiting for a message to arrive.
async function loadPeers() {
  const s = await api("/api/state");
  const before = JSON.stringify(state.peers);
  state.peers = s.nearby || {};
  renderAirspace(s);
  if (JSON.stringify(state.peers) !== before) {
    renderChats();
    renderThread();
  }
}

async function openChat(id) {
  state.open = id;
  // Keep the conversation in the URL so a reload, a bookmark or a second
  // window lands back where you were.
  if (location.hash.slice(1) !== id) history.replaceState(null, "", "#" + id);
  state.messages = [];
  state.typingUntil = 0;
  el.app.dataset.view = "thread";
  renderChats();
  renderThread();

  const data = await api(`/api/messages?chat=${encodeURIComponent(id)}&limit=200`);
  state.messages = data.messages || [];
  renderThread();
  el.messages.scrollTop = el.messages.scrollHeight;

  await api("/api/read", { chat: id });
  const chat = state.chats.find((c) => c.id === id);
  if (chat) { chat.unread = 0; renderChats(); }
  el.input.focus();
}

/* ------------------------------------------------------------------ events */

function connect() {
  const es = new EventSource("/api/events");

  es.onmessage = (e) => {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }

    switch (ev.type) {
      case "message": {
        if (ev.chat === state.open) {
          // Replace on id so an echo of our own optimistic row cannot double.
          const i = state.messages.findIndex((m) => m.id === ev.message.id);
          if (i >= 0) state.messages[i] = ev.message;
          else state.messages.push(ev.message);
          state.typingUntil = 0;
          renderThread();
          if (!ev.message.mine) api("/api/read", { chat: ev.chat }).catch(() => {});
        }
        loadState();
        break;
      }
      case "state": {
        const m = state.messages.find((m) => m.id === ev.id);
        if (m) { m.state = ev.state; renderThread(); }
        break;
      }
      case "deleted": {
        const m = state.messages.find((m) => m.id === ev.id);
        if (m) { m.deleted = true; m.body = ""; m.blob_id = ""; renderThread(); }
        loadState();
        break;
      }
      case "typing": {
        if (ev.chat === state.open) {
          state.typingUntil = ev.on ? Date.now() + 6000 : 0;
          renderThread();
        }
        break;
      }
      case "peers":
      case "contacts":
      case "chat":
        loadState();
        break;
    }
  };

  // EventSource reconnects on its own; resync when it does, because deltas
  // that arrived while we were disconnected are gone for good.
  es.onerror = () => setTimeout(() => loadState().catch(() => {}), 1500);
}

/* ------------------------------------------------------------------ wiring */

el.chats.addEventListener("click", (e) => {
  const row = e.target.closest("[data-chat]");
  if (row) openChat(row.dataset.chat);
});

el.messages.addEventListener("click", async (e) => {
  const del = e.target.closest("[data-del]");
  if (del) {
    await api("/api/delete", { id: del.dataset.del }).catch((err) => toast(err.message));
    return;
  }
  const img = e.target.closest("img");
  if (img) {
    el.lightboxImg.src = img.src;
    el.lightbox.classList.add("on");
  }
});

el.lightbox.addEventListener("click", () => el.lightbox.classList.remove("on"));

el.back.addEventListener("click", () => {
  state.open = null;
  history.replaceState(null, "", location.pathname);
  el.app.dataset.view = "list";
  renderChats();
  renderThread();
});

// Send. The textarea grows with the message, Enter sends, Shift+Enter breaks.
function autosize() {
  el.input.style.height = "auto";
  el.input.style.height = Math.min(el.input.scrollHeight, 168) + "px";
  el.send.disabled = !el.input.value.trim() && !state.attachment;
}
el.input.addEventListener("input", () => {
  autosize();
  sendTyping(true);
});
el.input.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    el.composer.requestSubmit();
  }
});

let typingTimer = null, typingOn = false;
function sendTyping(on) {
  if (!state.open) return;
  if (on && !typingOn) {
    typingOn = true;
    api("/api/typing", { chat: state.open, on: true }).catch(() => {});
  }
  clearTimeout(typingTimer);
  typingTimer = setTimeout(() => {
    if (typingOn) {
      typingOn = false;
      api("/api/typing", { chat: state.open, on: false }).catch(() => {});
    }
  }, 2500);
}

el.composer.addEventListener("submit", async (e) => {
  e.preventDefault();
  const body = el.input.value.trim();
  if (!body && !state.attachment) return;

  const payload = { to: state.open, body };
  if (state.attachment) Object.assign(payload, state.attachment);

  el.input.value = "";
  clearAttachment();
  autosize();

  try {
    await api("/api/send", payload);
    sendTyping(false);
  } catch (err) {
    toast(err.message);
    el.input.value = body;
    autosize();
  }
});

// Attachments. Images are downscaled in the browser, because Bluetooth moves a
// few kilobytes per second and a full-size photo would hold the radio for
// minutes and very likely fail partway.
el.attach.addEventListener("click", () => el.file.click());
el.attachClear.addEventListener("click", clearAttachment);

function clearAttachment() {
  state.attachment = null;
  el.file.value = "";
  el.attachPreview.hidden = true;
  el.attachThumb.hidden = true;
  el.attachThumb.removeAttribute("src");
  autosize();
}

el.file.addEventListener("change", async () => {
  const f = el.file.files[0];
  if (!f) return;
  try {
    const att = f.type.startsWith("image/") ? await shrinkImage(f) : await rawFile(f);
    state.attachment = att;
    el.attachName.textContent = `${f.name} · ${Math.round(att.data.length * 0.75 / 1024)} KB`;
    if (att.kind === "image") {
      el.attachThumb.src = `data:${att.mime};base64,${att.data}`;
      el.attachThumb.hidden = false;
    }
    el.attachPreview.hidden = false;
    autosize();
  } catch (err) {
    toast(err.message);
    clearAttachment();
  }
});

const MAX_ATTACHMENT = 96 * 1024;

function rawFile(f) {
  return new Promise((resolve, reject) => {
    if (f.size > MAX_ATTACHMENT) {
      reject(new Error(`That file is ${Math.round(f.size / 1024)} KB. Bluetooth can carry ${MAX_ATTACHMENT / 1024} KB.`));
      return;
    }
    const r = new FileReader();
    r.onload = () => resolve({
      kind: "file", mime: f.type || "application/octet-stream", name: f.name,
      data: r.result.split(",")[1],
    });
    r.onerror = () => reject(new Error("Could not read that file"));
    r.readAsDataURL(f);
  });
}

async function shrinkImage(f) {
  const bitmap = await createImageBitmap(f);
  // Step the quality and size down until it fits the link budget rather than
  // refusing the photo outright.
  for (const [maxDim, quality] of [[1280, 0.82], [1024, 0.72], [800, 0.62], [640, 0.5]]) {
    const scale = Math.min(1, maxDim / Math.max(bitmap.width, bitmap.height));
    const canvas = document.createElement("canvas");
    canvas.width = Math.round(bitmap.width * scale);
    canvas.height = Math.round(bitmap.height * scale);
    canvas.getContext("2d").drawImage(bitmap, 0, 0, canvas.width, canvas.height);

    const dataURL = canvas.toDataURL("image/jpeg", quality);
    const b64 = dataURL.split(",")[1];
    if (b64.length * 0.75 <= MAX_ATTACHMENT) {
      return { kind: "image", mime: "image/jpeg", name: f.name, data: b64 };
    }
  }
  throw new Error("That image is too detailed to send over Bluetooth");
}

// Paste an image straight into the composer.
document.addEventListener("paste", (e) => {
  if (!state.open) return;
  const item = [...(e.clipboardData?.items || [])].find((i) => i.type.startsWith("image/"));
  if (!item) return;
  const file = item.getAsFile();
  if (!file) return;
  e.preventDefault();
  const dt = new DataTransfer();
  dt.items.add(file);
  el.file.files = dt.files;
  el.file.dispatchEvent(new Event("change"));
});

// Identity
el.myAddress.addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(state.me.address);
    el.myAddress.classList.add("done");
    toast("Address copied. Hand it to someone to be reachable.");
    setTimeout(() => el.myAddress.classList.remove("done"), 1200);
  } catch {
    toast("Could not copy — select the address and copy it manually");
  }
});

el.myName.addEventListener("change", async () => {
  try {
    state.me = await api("/api/profile", { name: el.myName.value.trim() });
    toast("Name updated. People nearby will see it.");
  } catch (err) {
    toast(err.message);
  }
});

// Add someone
el.addContact.addEventListener("click", () => {
  el.addError.textContent = "";
  el.addAddress.value = "";
  el.addName.value = "";
  el.sheet.showModal();
  el.addAddress.focus();
});

el.addSave.addEventListener("click", async () => {
  const address = el.addAddress.value.trim();
  if (!address) { el.addError.textContent = "Paste an address to continue."; return; }
  try {
    const { contact } = await api("/api/contacts", { address, name: el.addName.value.trim() });
    el.sheet.close();
    await loadState();
    openChat(contact.node_id);
  } catch (err) {
    el.addError.textContent = err.message;
  }
});

el.addAddress.addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); el.addSave.click(); }
});

// Theme
const savedTheme = localStorage.getItem("yap.theme");
if (savedTheme) document.documentElement.dataset.theme = savedTheme;
el.themeToggle.addEventListener("click", () => {
  const next = document.documentElement.dataset.theme === "light" ? "dark" : "light";
  document.documentElement.dataset.theme = next;
  localStorage.setItem("yap.theme", next);
});

/* -------------------------------------------------------------------- boot */

loadState()
  .then(() => {
    const want = decodeURIComponent(location.hash.slice(1));
    if (want && state.chats.some((c) => c.id === want)) openChat(want);
  })
  .catch((err) => toast(err.message));
connect();
setInterval(() => loadPeers().catch(() => {}), 4000);
// Typing indicators expire on their own; re-render so a stale one clears.
setInterval(() => { if (state.typingUntil && Date.now() > state.typingUntil) renderThread(); }, 1000);
