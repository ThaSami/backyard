// Cursors: multiplayer cursors with names, click reactions and cursor chat.
//
// Everything is ephemeral and travels over one realtime room:
//   presence state {x, y, say}  x/y are fractions of the board (0..1) so
//                               different screen sizes line up; null = away
//   messages      {type: "react", emoji, x, y}
// Cursor updates are throttled to ~20/s with a trailing update.

// The SDK is loaded by index.html; without it (e.g. served by a plain static file server) say so.
if (!window.backyard) {
  document.querySelector("#status").textContent = "offline: open this page through Backyard (backyard dev or a deployed site)";
  throw new Error("The Backyard SDK (/__backyard/backyard.js) is not loaded");
}

const ROOM = "cursors";
const COLORS = ["#2a78d6", "#eb6834", "#1baf7a", "#6250d6", "#e87ba4", "#008300", "#e34948", "#c98500"];
const EMOJIS = ["👋", "🎉", "❤️", "🔥", "👀", "🚀"];
const THROTTLE_MS = 50;

const $ = (sel) => document.querySelector(sel);
const board = $("#board");
const colorFor = (id) => COLORS[[...String(id)].reduce((h, c) => (h * 31 + c.charCodeAt(0)) >>> 0, 7) % COLORS.length];

let me = null;
let emoji = EMOJIS[0];
let pos = { x: null, y: null }; // my last position (fractions)
const cursors = new Map(); // cid → element

const room = backyard.socket.room(ROOM, { state: { x: null, y: null, say: "" } });

// ---------------------------------------------------------------------------
// Publishing my cursor (throttled)

let pending = null;
let timer = null;
function publish(partial) {
  pending = { ...pending, ...partial };
  if (timer) return;
  room.updateState(pending);
  pending = null;
  timer = setTimeout(() => {
    timer = null;
    if (pending) publish({});
  }, THROTTLE_MS);
}

function fraction(e) {
  const r = board.getBoundingClientRect();
  return { x: Math.min(1, Math.max(0, (e.clientX - r.left) / r.width)), y: Math.min(1, Math.max(0, (e.clientY - r.top) / r.height)) };
}

board.addEventListener("pointermove", (e) => {
  pos = fraction(e);
  publish(pos);
  if (!$("#chat").hidden) placeChat();
});
board.addEventListener("pointerleave", () => {
  if ($("#chat").hidden) publish({ x: null, y: null });
});
board.addEventListener("click", (e) => {
  const p = fraction(e);
  room.send({ type: "react", emoji, x: p.x, y: p.y }, { echo: true });
});

// ---------------------------------------------------------------------------
// Rendering other people's cursors

function cursorElement(m) {
  const el = document.createElement("div");
  el.className = "cursor";
  el.style.setProperty("--c", colorFor(m.user.id));
  el.innerHTML = `<svg width="20" height="22" viewBox="0 0 20 22" aria-hidden="true"><path d="M2 2l16 8.5-7 1.8-3.3 7.2z"/></svg>
    <span class="tag"><span class="who"></span><span class="say"></span></span>`;
  el.querySelector(".who").textContent = m.user.name;
  board.append(el);
  return el;
}

function renderCursors(members) {
  const r = board.getBoundingClientRect();
  const live = new Set();
  for (const m of members) {
    const { x, y, say } = m.state ?? {};
    if (typeof x !== "number" || typeof y !== "number") continue;
    live.add(m.cid);
    const el = cursors.get(m.cid) ?? cursors.set(m.cid, cursorElement(m)).get(m.cid);
    el.style.transform = `translate(${x * r.width}px, ${y * r.height}px)`;
    const sayEl = el.querySelector(".say");
    sayEl.textContent = typeof say === "string" ? say : "";
    el.classList.toggle("talking", Boolean(sayEl.textContent));
  }
  for (const [cid, el] of cursors) {
    if (!live.has(cid)) {
      el.remove();
      cursors.delete(cid);
    }
  }
}

function renderPeople(members) {
  const people = new Map();
  if (me) people.set(me.id, { user: me, tabs: 1, you: true });
  for (const m of members) {
    const p = people.get(m.user.id);
    if (p) p.tabs++;
    else people.set(m.user.id, { user: m.user, tabs: 1 });
  }
  $("#people").replaceChildren(
    ...[...people.values()].map(({ user, tabs, you }) => {
      const li = document.createElement("li");
      const dot = Object.assign(document.createElement("span"), { className: "dot" });
      dot.style.background = colorFor(user.id);
      const name = Object.assign(document.createElement("span"), { textContent: user.name + (you ? " (you)" : "") });
      li.append(dot, name);
      if (tabs > 1) li.append(Object.assign(document.createElement("small"), { textContent: ` ×${tabs}` }));
      return li;
    }),
  );
}

room.on("members", (members) => {
  renderCursors(members);
  renderPeople(members);
});
room.on("join", (m) => toast(`${m.user.name} joined`));
room.on("leave", (m) => toast(`${m.user.name} left`));

// Reactions float up from where they were clicked.
room.on("message", (data) => {
  if (data?.type !== "react" || typeof data.x !== "number" || typeof data.y !== "number") return;
  const r = board.getBoundingClientRect();
  const el = Object.assign(document.createElement("span"), { className: "reaction", textContent: String(data.emoji).slice(0, 4) });
  el.style.left = `${data.x * r.width}px`;
  el.style.top = `${data.y * r.height}px`;
  board.append(el);
  el.addEventListener("animationend", () => el.remove());
});

// Keep cursors in place when the window is resized.
addEventListener("resize", () => renderCursors(room.members));

// ---------------------------------------------------------------------------
// Cursor chat: "/" opens a small input next to my cursor; the text is part of
// my presence state, so it disappears when I leave.

function placeChat() {
  const r = board.getBoundingClientRect();
  const chat = $("#chat");
  chat.style.left = `${r.left + (pos.x ?? 0.5) * r.width + 18}px`;
  chat.style.top = `${r.top + (pos.y ?? 0.5) * r.height + 18}px`;
}

let sayTimer = null;

addEventListener("keydown", (e) => {
  const chat = $("#chat");
  if (e.key === "/" && chat.hidden) {
    e.preventDefault();
    clearTimeout(sayTimer);
    chat.hidden = false;
    chat.value = "";
    placeChat();
    chat.focus();
  } else if (e.key === "Escape" && !chat.hidden) {
    chat.hidden = true;
    publish({ say: "" });
  }
});
$("#chat").addEventListener("input", (e) => publish({ say: e.target.value }));
$("#chat").addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.target.hidden = true;
    const said = e.target.value;
    sayTimer = setTimeout(() => publish({ say: "" }), 4000 + said.length * 60);
  }
});

// ---------------------------------------------------------------------------
// Toolbar, toasts, connection status

$("#emojis").append(
  ...EMOJIS.map((em) => {
    const b = Object.assign(document.createElement("button"), { textContent: em, title: `React with ${em}` });
    b.classList.toggle("active", em === emoji);
    b.onclick = () => {
      emoji = em;
      document.querySelectorAll("#emojis button").forEach((x) => x.classList.toggle("active", x === b));
    };
    return b;
  }),
);

function toast(text) {
  const el = Object.assign(document.createElement("div"), { className: "toast", textContent: text });
  $("#toasts").append(el);
  setTimeout(() => el.remove(), 3000);
}

backyard.socket.on("open", () => ($("#status").textContent = "live"));
backyard.socket.on("close", () => ($("#status").textContent = "reconnecting…"));

backyard
  .me()
  .then((user) => {
    me = user;
    renderPeople(room.members);
  })
  .catch((err) => toast(`Could not load your profile: ${err.message}`));
