// App logic. The Backyard SDK (window.backyard) is loaded by index.html before
// this module runs. API cheat sheet: CLAUDE.md / AGENTS.md, or the Backyard
// skill in .claude/skills/backyard/ (references/api.md has every detail).
//
// Data model: one shared collection "items" with documents
//   { text: string, done: boolean }  (+ id and _meta added by Backyard)
// Everyone who opens the site sees the same items, live.

const $ = (sel) => document.querySelector(sel);

if (!window.backyard) {
  $("#loading").textContent = "Backyard is not available here. Open this page with `backyard dev` or after `backyard deploy`.";
  throw new Error("The Backyard SDK (/__backyard/backyard.js) is not loaded");
}

const items = backyard.db.collection("items");

const state = {
  me: null, // the signed-in user (backyard.me())
  items: [], // latest result of the live query
  filter: "all", // all | open | mine
};

// ---------------------------------------------------------------------------
// Rendering

function initials(name) {
  return (name || "?").split(/\s+/).filter(Boolean).map((w) => w[0]).slice(0, 2).join("").toUpperCase();
}

function avatar(el, person) {
  el.replaceChildren();
  if (person.avatar_url) {
    el.append(Object.assign(new Image(), { src: person.avatar_url, alt: "" }));
  } else {
    el.textContent = initials(person.name);
  }
}

function visibleItems() {
  switch (state.filter) {
    case "open":
      return state.items.filter((it) => !it.done);
    case "mine":
      return state.items.filter((it) => it._meta.created_by === state.me.id);
    default:
      return state.items;
  }
}

function render() {
  const list = visibleItems();
  $("#loading").hidden = true;
  $("#empty").hidden = list.length > 0;
  $("#list").replaceChildren(
    ...list.map((it) => {
      const li = document.createElement("li");
      li.className = "item" + (it.done ? " done" : "");
      li.innerHTML = `
        <label><input type="checkbox"><span class="text"></span></label>
        <span class="meta"></span>
        <button class="icon" title="Delete" aria-label="Delete">✕</button>`;
      const box = li.querySelector("input");
      box.checked = Boolean(it.done);
      box.onchange = () => run(items.update(it.id, { done: box.checked }));
      li.querySelector(".text").textContent = it.text; // textContent: never inject user HTML
      li.querySelector(".meta").textContent = it.author ?? "";
      li.querySelector("button").onclick = () => run(items.delete(it.id));
      return li;
    }),
  );
}

function toast(message) {
  const el = $("#toast");
  el.textContent = message;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (el.hidden = true), 4000);
}

// run reports a failed write instead of leaving an unhandled rejection; it
// resolves to false when the write failed.
function run(promise) {
  return promise.then(
    () => true,
    (err) => {
      toast(err.code === "rate_limited" ? "Too many changes, slow down a little." : `Could not save: ${err.message}`);
      return false;
    },
  );
}

// ---------------------------------------------------------------------------
// Startup

async function main() {
  state.me = await backyard.me();
  $("#user").hidden = false;
  $("#user-name").textContent = state.me.name;
  avatar($("#user-avatar"), state.me);

  // Live query: called now and after every change by anyone (debounced).
  items.watch(
    (docs) => {
      state.items = docs;
      render();
    },
    items.orderBy("_meta.created_at", "desc").limit(200),
  );

  // Presence: how many people (not tabs) have the page open.
  const room = backyard.socket.room("presence");
  room.on("members", (members) => {
    const people = new Set([state.me.id, ...members.map((m) => m.user.id)]).size;
    $("#online").hidden = false;
    $("#online").textContent = `● ${people} online`;
  });
  backyard.socket.on("close", () => ($("#offline").hidden = false));
  backyard.socket.on("open", () => ($("#offline").hidden = true));

  $("#add-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const input = $("#add-input");
    const text = input.value.trim();
    if (!text) return;
    input.value = "";
    if (!(await run(items.create({ text, done: false, author: state.me.name }))) && !input.value) {
      input.value = text; // keep the text so it can be retried
    }
  });

  document.querySelectorAll("[data-filter]").forEach((chip) => {
    chip.addEventListener("click", () => {
      state.filter = chip.dataset.filter;
      document.querySelectorAll("[data-filter]").forEach((c) => {
        c.classList.toggle("active", c === chip);
        c.setAttribute("aria-pressed", String(c === chip));
      });
      render();
    });
  });
}

main().catch((err) => {
  $("#loading").textContent = `Could not reach Backyard: ${err.message}`;
});
