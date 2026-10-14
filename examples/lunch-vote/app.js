// Lunch Vote: realtime voting with presence.
//
// Data (shared by everyone who opens the site):
//   spots  {name, emoji}                          id = slug of the name (duplicates are rejected)
//   votes  {day, spot, user:{id,name,avatar_url}}  id = "<day>.<user>" → one vote per person per day
// Presence: room "lunch-<day>" shows who is deciding right now.

// The SDK is loaded by index.html; without it (e.g. served by a plain static file server) say so.
if (!window.backyard) {
  document.querySelector("#loading").textContent = "Open this page through Backyard (backyard dev or a deployed site).";
  throw new Error("The Backyard SDK (/__backyard/backyard.js) is not loaded");
}

const spots = backyard.db.collection("spots");
const votes = backyard.db.collection("votes");

const EMOJI = ["🍜", "🌮", "🍕", "🍣", "🥗", "🍔", "🥙", "🍛", "🥪", "🍲", "🥟", "🍝", "🌯", "🍱"];
const today = new Date().toLocaleDateString("en-CA"); // YYYY-MM-DD, local time
const state = { me: null, spots: [], votes: [], members: [], loaded: 0 };

const $ = (sel) => document.querySelector(sel);
// Document ids allow only A-Z a-z 0-9 _ - : .
const key = (s) => String(s).replace(/[^A-Za-z0-9_\-:.]/g, "_").slice(0, 120);
const myVoteId = () => `${today}.${key(state.me.id)}`;

function initials(name) {
  return (name || "?").split(/\s+/).filter(Boolean).map((w) => w[0]).slice(0, 2).join("").toUpperCase();
}

// A stable color per person for initials avatars.
// Every color keeps white text at ≥ 4.5:1 contrast (WCAG AA).
const COLORS = ["#2266c8", "#c4501d", "#0f7f57", "#6250d6", "#b8467a", "#007a00", "#c62f2e", "#9a6500"];
const colorFor = (id) => COLORS[[...String(id)].reduce((h, c) => (h * 31 + c.charCodeAt(0)) >>> 0, 7) % COLORS.length];

function avatar(user, size = "md") {
  const el = user.avatar_url ? Object.assign(new Image(), { src: user.avatar_url, alt: "" }) : document.createElement("span");
  el.className = `avatar ${size}`;
  el.title = user.name;
  if (!user.avatar_url) {
    el.textContent = initials(user.name);
    el.style.background = colorFor(user.id);
  }
  return el;
}

function showError(err) {
  const el = $("#error");
  el.hidden = false;
  el.textContent = err.code === "rate_limited" ? "Easy there! Try again in a moment." : err.message;
  clearTimeout(showError.timer);
  showError.timer = setTimeout(() => (el.hidden = true), 5000);
}

// ---------------------------------------------------------------------------
// Rendering

function tally() {
  const byspot = new Map();
  for (const v of state.votes) {
    if (!byspot.has(v.spot)) byspot.set(v.spot, []);
    byspot.get(v.spot).push(v);
  }
  return byspot;
}

function render() {
  if (state.loaded !== 3) return; // wait for both live queries (bit 1: spots, bit 2: votes)
  const byspot = tally();
  const count = (s) => byspot.get(s.id)?.length ?? 0;
  const mine = state.votes.find((v) => v.id === myVoteId())?.spot ?? null;
  const max = Math.max(1, ...state.spots.map(count));
  const sorted = [...state.spots].sort((a, b) => count(b) - count(a) || a.name.localeCompare(b.name));

  $("#loading").hidden = true;
  $("#empty").hidden = sorted.length > 0;

  // Leader banner
  const leaders = sorted.filter((s) => count(s) === count(sorted[0]) && count(s) > 0);
  const leader = $("#leader");
  leader.hidden = leaders.length === 0;
  if (leaders.length === 1) {
    const n = count(leaders[0]);
    leader.textContent = `${leaders[0].emoji ?? "🍽️"} ${leaders[0].name} is winning with ${n} vote${n === 1 ? "" : "s"}`;
  } else if (leaders.length > 1) {
    leader.textContent = `🤝 It's a tie: ${leaders.map((s) => s.name).join(" vs ")}`;
  }

  $("#spots").replaceChildren(
    ...sorted.map((s) => {
      const n = count(s);
      const li = document.createElement("li");
      li.className = "spot" + (s.id === mine ? " mine" : "") + (n > 0 && n === count(sorted[0]) ? " lead" : "");

      const btn = document.createElement("button");
      btn.className = "vote";
      btn.setAttribute("aria-pressed", String(s.id === mine));
      btn.title = s.id === mine ? "Take back your vote" : `Vote for ${s.name}`;
      btn.innerHTML = `<span class="emoji" aria-hidden="true"></span>
        <span class="info"><span class="name"></span><span class="bar"><span></span></span></span>
        <span class="voters"></span><span class="count"></span>`;
      btn.querySelector(".emoji").textContent = s.emoji ?? "🍽️";
      btn.querySelector(".name").textContent = s.name;
      btn.querySelector(".bar span").style.width = `${(n / max) * 100}%`;
      btn.querySelector(".count").textContent = n;
      const voters = byspot.get(s.id) ?? [];
      btn.querySelector(".voters").append(...voters.slice(0, 5).map((v) => avatar(v.user, "sm")));
      if (voters.length > 5) btn.querySelector(".voters").append(Object.assign(document.createElement("span"), { className: "more", textContent: `+${voters.length - 5}` }));
      btn.onclick = () => vote(s.id, mine);
      li.append(btn);

      // Places can be removed while nobody votes for them (the slot is kept for alignment).
      const rm = Object.assign(document.createElement("button"), { className: "remove", textContent: "✕", title: `Remove ${s.name}` });
      rm.setAttribute("aria-label", `Remove ${s.name}`);
      if (n > 0) rm.classList.add("locked");
      rm.disabled = n > 0;
      rm.onclick = () => spots.delete(s.id).catch(showError);
      li.append(rm);
      return li;
    }),
  );
  renderPresence();
}

function renderPresence() {
  if (!state.me) return;
  // One avatar per person (a person may have several tabs open), you first.
  const people = new Map([[state.me.id, state.me]]);
  for (const m of state.members) if (!people.has(m.user.id)) people.set(m.user.id, m.user);
  const voted = new Set(state.votes.map((v) => v.user?.id));
  $("#stack").replaceChildren(
    ...[...people.values()].slice(0, 8).map((u) => {
      const a = avatar(u);
      if (voted.has(u.id)) a.classList.add("voted");
      return a;
    }),
  );
  const n = people.size;
  $("#here").textContent = n === 1 ? "just you so far" : `${n} deciding`;
}

// ---------------------------------------------------------------------------
// Actions

async function vote(spotId, current) {
  try {
    if (current === spotId) {
      await votes.delete(myVoteId());
    } else {
      const { id, name, avatar_url } = state.me;
      await votes.set(myVoteId(), { day: today, spot: spotId, user: { id, name, avatar_url: avatar_url ?? null } });
    }
  } catch (err) {
    showError(err);
  }
}

$("#add").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = $("#name").value.trim().replace(/\s+/g, " ");
  if (!name) return;
  const button = e.target.querySelector("button");
  button.disabled = true;
  try {
    await spots.create({ name, emoji: $("#emoji").value }, { id: key(name.toLowerCase()) });
    $("#name").value = "";
  } catch (err) {
    if (err.code === "conflict") showError({ message: `${name} is already on the list.` });
    else showError(err);
  } finally {
    button.disabled = false;
  }
});

// ---------------------------------------------------------------------------
// Startup

async function main() {
  $("#emoji").append(...EMOJI.map((e) => Object.assign(document.createElement("option"), { value: e, textContent: e })));
  $("#today").textContent = new Date().toLocaleDateString(undefined, { weekday: "long", month: "long", day: "numeric" });

  state.me = await backyard.me();
  $("#who").textContent = `voting as ${state.me.name}`;

  spots.watch(
    (docs) => {
      state.spots = docs;
      state.loaded |= 1;
      render();
    },
    spots.orderBy("name").limit(200),
  );
  votes.watch(
    (docs) => {
      state.votes = docs;
      state.loaded |= 2;
      render();
    },
    votes.where("day", "==", today).limit(1000),
  );

  const room = backyard.socket.room(`lunch-${today}`);
  room.on("members", (members) => {
    state.members = members;
    renderPresence();
  });

  // Votes are per day: start fresh after midnight.
  const midnight = new Date();
  midnight.setHours(24, 0, 5, 0);
  setTimeout(() => location.reload(), midnight - Date.now());
}

main().catch((err) => {
  $("#loading").textContent = `Could not load: ${err.message}`;
});
