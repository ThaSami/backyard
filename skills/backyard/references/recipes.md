# Backyard recipes

Complete, copy-paste apps. Each recipe is a single `index.html`: save it in an empty folder,
run `backyard dev` to try it locally and `backyard deploy --site <name>` to publish. They use
only the documented SDK (see [api.md](api.md)) and no external libraries.

Conventions used throughout:

- The SDK is loaded with `<script src="/__backyard/backyard.js"></script>`; app code runs in a
  `<script type="module">` after it, so it can use top-level `await`.
- User content is inserted with `textContent` (or escaped with `esc()`), never raw `innerHTML`.
- `key(s)` turns an email into a valid document id: ids allow only `A-Z a-z 0-9 _ - : .`.
- Lists are rendered from `watch(...)`, so every open tab updates live.

| # | Recipe | APIs |
|---|---|---|
| 1 | [Lunch-spot voting](#1-lunch-spot-voting) | db, watch, rooms |
| 2 | [Live all-hands poll](#2-live-all-hands-poll) | db, watch, rooms |
| 3 | [Multiplayer cursors](#3-multiplayer-cursors) | rooms, presence, messages |
| 4 | [Chat room with presence](#4-chat-room-with-presence) | db, watch, rooms |
| 5 | [AI summarizer (streaming)](#5-ai-summarizer-streaming) | ai.stream |
| 6 | [Image generator gallery](#6-image-generator-gallery) | ai.image, db |
| 7 | [File drop and gallery](#7-file-drop-and-gallery) | files, db |
| 8 | [Kanban board](#8-kanban-board) | db, watch |
| 9 | [Warehouse KPI dashboard](#9-warehouse-kpi-dashboard) | warehouse, SVG |
| 10 | [Guestbook](#10-guestbook) | db, watch, identity |
| 11 | [Game leaderboard](#11-game-leaderboard) | db, watch, rooms |
| 12 | [Sharing code between sites](#12-sharing-code-between-sites) | absolute URLs |

---

## 1. Lunch-spot voting

Everyone adds places and votes once per day; results re-sort live and show how many people
are deciding right now. Data: `spots` (`{name}`, id derived from the name so duplicates are
rejected) and `votes` (one doc per person per day: id `<day>.<user>`).

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Lunch Vote</title>
  <meta name="description" content="Vote where the team eats today, live">
  <style>
    :root { color-scheme: light dark; font: 16px/1.5 system-ui, sans-serif; --accent: #2a78d6; --line: #8884; }
    body { max-width: 560px; margin: 2rem auto; padding: 0 1rem; }
    button, input { font: inherit; }
    ul { list-style: none; padding: 0; }
    li { padding: .6rem 0; border-bottom: 1px solid var(--line); }
    .row { display: flex; align-items: center; gap: .75rem; }
    .row b { flex: 1; }
    li.mine b::after { content: " ✓"; color: var(--accent); }
    .bar { height: 6px; margin-top: .35rem; border-radius: 3px; background: var(--accent); transition: width .3s; }
    form { display: flex; gap: .5rem; } input { flex: 1; }
    #error { color: #d03b3b; }
  </style>
</head>
<body>
  <h1>Where do we eat today?</h1>
  <p id="status">Loading…</p>
  <ul id="spots"></ul>
  <form id="add"><input id="place" placeholder="Add a place…" maxlength="60" required><button>Add</button></form>
  <p id="error"></p>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const spots = backyard.db.collection("spots");
    const votes = backyard.db.collection("votes");
    const key = (s) => s.replace(/[^A-Za-z0-9_\-:.]/g, "_").slice(0, 120);
    const today = new Date().toLocaleDateString("en-CA");           // YYYY-MM-DD in local time
    const me = await backyard.me();
    const myVoteId = `${today}.${key(me.id)}`;
    let spotDocs = [], voteDocs = [], here = 1;

    spots.watch((docs) => { spotDocs = docs; render(); }, spots.orderBy("name").limit(200));
    votes.watch((docs) => { voteDocs = docs; render(); }, votes.where("day", "==", today).limit(1000));

    // Presence: count the people (not tabs) who have the page open.
    const room = backyard.socket.room("lunch-" + today);
    room.on("members", (members) => { here = new Set([me.id, ...members.map((m) => m.user.id)]).size; render(); });

    function render() {
      const counts = {};
      for (const v of voteDocs) counts[v.spot] = (counts[v.spot] || 0) + 1;
      const mine = voteDocs.find((v) => v.id === myVoteId)?.spot;
      const max = Math.max(1, ...Object.values(counts));
      const sorted = [...spotDocs].sort((a, b) => (counts[b.id] || 0) - (counts[a.id] || 0));
      document.querySelector("#status").textContent =
        `${voteDocs.length} vote${voteDocs.length === 1 ? "" : "s"} today · ${here} here now · voting as ${me.name}`;
      document.querySelector("#spots").replaceChildren(...sorted.map((s) => {
        const n = counts[s.id] || 0;
        const li = document.createElement("li");
        li.className = s.id === mine ? "mine" : "";
        li.innerHTML = `<div class="row"><b></b><span></span><button></button></div><div class="bar"></div>`;
        li.querySelector("b").textContent = s.name;
        li.querySelector("span").textContent = n;
        li.querySelector(".bar").style.width = `${(n / max) * 100}%`;
        const btn = li.querySelector("button");
        btn.textContent = s.id === mine ? "Unvote" : "Vote";
        btn.onclick = () => s.id === mine
          ? votes.delete(myVoteId)
          : votes.set(myVoteId, { day: today, spot: s.id, name: me.name });
        return li;
      }));
    }

    document.querySelector("#add").onsubmit = async (e) => {
      e.preventDefault();
      const input = document.querySelector("#place");
      const name = input.value.trim();
      const error = document.querySelector("#error");
      error.textContent = "";
      if (!name) return;
      try {
        await spots.create({ name }, { id: key(name.toLowerCase()) });
        input.value = "";
      } catch (err) {
        error.textContent = err.code === "conflict" ? `${name} is already on the list.` : err.message;
      }
    };
  </script>
</body>
</html>
```

---

## 2. Live all-hands poll

The host opens `…/?host`, types a question and options, and starts a round; the audience
opens the plain URL on their phones and votes. Results update live for everyone. Data: `poll`
(a single doc `current`: `{question, options, round, open}`) and `answers` (one doc per person
per round).

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Live Poll</title>
  <meta name="description" content="Ask the room a question and watch the answers arrive">
  <style>
    :root { color-scheme: light dark; font: 17px/1.5 system-ui, sans-serif; --accent: #2a78d6; --line: #8884; }
    body { max-width: 680px; margin: 2rem auto; padding: 0 1rem; }
    button, input, textarea { font: inherit; }
    textarea, input { width: 100%; box-sizing: border-box; margin: .25rem 0 .75rem; }
    .opt { display: block; width: 100%; text-align: left; padding: .7rem 1rem; margin: .4rem 0; border-radius: 10px;
           border: 1px solid var(--line); background: none; cursor: pointer; }
    .opt.chosen { border-color: var(--accent); box-shadow: inset 0 0 0 1px var(--accent); }
    .result { margin: .6rem 0; }
    .result .label { display: flex; justify-content: space-between; }
    .track { height: 10px; border-radius: 5px; background: #8882; }
    .fill { height: 100%; border-radius: 5px; background: var(--accent); transition: width .4s; }
    .muted { opacity: .7; }
  </style>
</head>
<body>
  <main id="app">Loading…</main>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const isHost = new URLSearchParams(location.search).has("host");
    const polls = backyard.db.collection("poll");
    const answers = backyard.db.collection("answers");
    const key = (s) => s.replace(/[^A-Za-z0-9_\-:.]/g, "_").slice(0, 120);
    const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
    const app = document.querySelector("#app");
    const me = await backyard.me();
    let poll = null, results = [], stopAnswers = null, here = 1;

    polls.watch(([doc]) => {
      const newRound = doc?.round !== poll?.round;
      poll = doc ?? null;
      if (newRound) {
        stopAnswers?.();
        results = [];
        stopAnswers = poll
          ? answers.watch((docs) => { results = docs; render(); }, answers.where("round", "==", poll.round).limit(1000))
          : null;
      }
      render();
    }, polls.where("id", "==", "current"));

    const room = backyard.socket.room("poll");
    room.on("members", (m) => { here = new Set([me.id, ...m.map((x) => x.user.id)]).size; render(); });

    const myAnswerId = () => `${poll.round}.${key(me.id)}`;

    function resultsHTML() {
      const counts = poll.options.map((_, i) => results.filter((a) => a.choice === i).length);
      const total = results.length || 1;
      return poll.options.map((opt, i) => `
        <div class="result"><div class="label"><span>${esc(opt)}</span><b>${Math.round((counts[i] / total) * 100)}% · ${counts[i]}</b></div>
        <div class="track"><div class="fill" style="width:${(counts[i] / total) * 100}%"></div></div></div>`).join("")
        + `<p class="muted">${results.length} answer${results.length === 1 ? "" : "s"} · ${here} watching</p>`;
    }

    function render() {
      if (isHost) return renderHost();
      if (!poll) { app.innerHTML = `<h1>Waiting for the host…</h1>`; return; }
      const mine = results.find((a) => a.id === myAnswerId())?.choice;
      app.innerHTML = `<h1>${esc(poll.question)}</h1>
        ${poll.options.map((o, i) => `<button class="opt ${mine === i ? "chosen" : ""}" data-i="${i}" ${poll.open ? "" : "disabled"}>${esc(o)}</button>`).join("")}
        ${mine !== undefined || !poll.open ? resultsHTML() : `<p class="muted">Pick one — you can change your mind until voting closes.</p>`}`;
      app.querySelectorAll(".opt").forEach((b) => b.onclick = () =>
        answers.set(myAnswerId(), { round: poll.round, choice: Number(b.dataset.i), name: me.name }));
    }

    function renderHost() {
      if (!app.querySelector("#new")) {                    // build the form once so typing is never interrupted
        const audience = location.origin + location.pathname;
        app.innerHTML = `<h1>Host</h1>
          <p class="muted">Audience link: <a href="${esc(audience)}">${esc(audience)}</a></p>
          <form id="new"><label>Question<input name="q" required maxlength="200" placeholder="What should we demo next?"></label>
          <label>Options (one per line)<textarea name="o" rows="4" required></textarea></label>
          <button>Start new round</button> <button type="button" id="close">Close voting</button></form>
          <section id="live"></section>`;
        app.querySelector("#new").onsubmit = async (e) => {
          e.preventDefault();
          const f = new FormData(e.target);
          const options = String(f.get("o")).split("\n").map((s) => s.trim()).filter(Boolean).slice(0, 10);
          if (options.length < 2) return alert("Give at least two options.");
          await polls.set("current", { question: String(f.get("q")).trim(), options, round: (poll?.round ?? 0) + 1, open: true });
          e.target.reset();
        };
        app.querySelector("#close").onclick = () => polls.update("current", { open: false });
      }
      app.querySelector("#close").hidden = !poll?.open;
      app.querySelector("#live").innerHTML = poll
        ? `<h2>${esc(poll.question)} ${poll.open ? "" : "<small>(closed)</small>"}</h2>${resultsHTML()}` : "";
    }
  </script>
</body>
</html>
```

---

## 3. Multiplayer cursors

Everyone's pointer is shared through room presence (positions are fractions of the window, so
different screen sizes line up); clicks broadcast a "ping" ripple as a room message. Updates
are throttled to 20/s with a trailing update so the last position is never lost.

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Cursors</title>
  <style>
    :root { color-scheme: light dark; font: 14px/1.4 system-ui, sans-serif; }
    body { margin: 0; height: 100vh; overflow: hidden; cursor: crosshair; }
    #hint { position: fixed; inset: auto 0 1rem; text-align: center; opacity: .6; }
    .cursor { position: fixed; left: 0; top: 0; pointer-events: none; transition: transform 90ms linear; }
    .cursor span { position: absolute; left: 14px; top: 16px; padding: 1px 8px; border-radius: 9px; color: #fff; white-space: nowrap; }
    .ping { position: fixed; width: 12px; height: 12px; margin: -6px; border-radius: 50%; pointer-events: none;
            border: 2px solid; animation: ping .8s ease-out forwards; }
    @keyframes ping { to { transform: scale(6); opacity: 0; } }
  </style>
</head>
<body>
  <p id="hint">Move your pointer — everyone sees it. Click to ping.</p>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const palette = ["#2a78d6", "#eb6834", "#1baf7a", "#6250d6", "#e87ba4", "#008300", "#e34948", "#eda100"];
    const colorFor = (s) => palette[[...s].reduce((h, c) => (h * 31 + c.charCodeAt(0)) >>> 0, 0) % palette.length];
    const room = backyard.socket.room("cursors", { state: { x: -1, y: -1 } });
    const cursors = new Map();                                           // cid → element

    room.on("members", (members) => {
      const live = new Set();
      for (const m of members) {
        if (!(m.state.x >= 0)) continue;                                 // pointer outside the window
        live.add(m.cid);
        let el = cursors.get(m.cid);
        if (!el) {
          const color = colorFor(m.user.id);
          el = document.createElement("div");
          el.className = "cursor";
          el.innerHTML = `<svg width="18" height="22" viewBox="0 0 18 22"><path d="M1 1l16 9-7 2-3 8z" fill="${color}" stroke="#fff" stroke-width="1.5"/></svg><span></span>`;
          el.querySelector("span").textContent = m.user.name;
          el.querySelector("span").style.background = color;
          document.body.append(el);
          cursors.set(m.cid, el);
        }
        el.style.transform = `translate(${m.state.x * innerWidth}px, ${m.state.y * innerHeight}px)`;
      }
      for (const [cid, el] of cursors) if (!live.has(cid)) { el.remove(); cursors.delete(cid); }
    });

    // Throttle to one update per 50 ms, plus a trailing update.
    let pending = null, timer = null;
    function publish(pos) {
      pending = pos;
      if (timer) return;
      room.updateState(pending); pending = null;
      timer = setTimeout(function flush() {
        timer = null;
        if (pending) publish(pending);
      }, 50);
    }
    addEventListener("pointermove", (e) => publish({ x: e.clientX / innerWidth, y: e.clientY / innerHeight }));
    document.documentElement.addEventListener("pointerleave", () => publish({ x: -1, y: -1 }));

    addEventListener("click", (e) => room.send({ type: "ping", x: e.clientX / innerWidth, y: e.clientY / innerHeight }, { echo: true }));
    room.on("message", (data, from) => {
      if (data?.type !== "ping") return;
      const el = document.createElement("div");
      el.className = "ping";
      el.style.cssText = `left:${data.x * innerWidth}px; top:${data.y * innerHeight}px; border-color:${colorFor(from.user.id)}`;
      document.body.append(el);
      setTimeout(() => el.remove(), 900);
    });
  </script>
</body>
</html>
```

---

## 4. Chat room with presence

Messages are stored in the db (so late joiners see history); presence shows who is online and
who is typing. The typing flag is only sent when it changes.

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Team Chat</title>
  <style>
    :root { color-scheme: light dark; font: 15px/1.45 system-ui, sans-serif; --accent: #2a78d6; --line: #8884; }
    body { max-width: 720px; height: 100vh; margin: 0 auto; display: flex; flex-direction: column; padding: 0 1rem; box-sizing: border-box; }
    header { padding: .75rem 0; border-bottom: 1px solid var(--line); }
    #people { font-size: .85em; opacity: .75; }
    #log { flex: 1; overflow-y: auto; padding: .5rem 0; }
    .msg { margin: .35rem 0; max-width: 80%; }
    .msg.mine { margin-left: auto; text-align: right; }
    .msg .meta { font-size: .75em; opacity: .65; }
    .msg .text { display: inline-block; padding: .4rem .75rem; border-radius: 14px; background: #8882; white-space: pre-wrap; text-align: left; }
    .msg.mine .text { background: var(--accent); color: #fff; }
    #typing { min-height: 1.3em; font-size: .85em; opacity: .7; }
    form { display: flex; gap: .5rem; padding: .5rem 0 1rem; }
    input { flex: 1; font: inherit; padding: .5rem .75rem; } button { font: inherit; }
  </style>
</head>
<body>
  <header><b>#general</b> <span id="people"></span></header>
  <div id="log"></div>
  <div id="typing"></div>
  <form id="send"><input id="text" placeholder="Message…" autocomplete="off" maxlength="2000"><button>Send</button></form>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const messages = backyard.db.collection("messages");
    const me = await backyard.me();
    const room = backyard.socket.room("chat", { state: { typing: false } });
    const log = document.querySelector("#log");
    const input = document.querySelector("#text");

    messages.watch((docs) => {
      const nearBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 80;
      log.replaceChildren(...docs.slice().reverse().map((m) => {
        const div = document.createElement("div");
        div.className = "msg" + (m._meta.created_by === me.id ? " mine" : "");
        div.innerHTML = `<div class="meta"></div><div class="text"></div>`;
        div.querySelector(".meta").textContent = `${m.name} · ${new Date(m._meta.created_at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
        div.querySelector(".text").textContent = m.text;
        return div;
      }));
      if (nearBottom) log.scrollTop = log.scrollHeight;
    }, messages.orderBy("_meta.created_at", "desc").limit(100));

    room.on("members", (members) => {
      const names = new Map([[me.id, me.name], ...members.map((m) => [m.user.id, m.user.name])]);
      document.querySelector("#people").textContent = `· ${names.size} online: ${[...names.values()].join(", ")}`;
      const typing = [...new Set(members.filter((m) => m.state.typing).map((m) => m.user.name))];
      document.querySelector("#typing").textContent = typing.length ? `${typing.join(", ")} ${typing.length > 1 ? "are" : "is"} typing…` : "";
    });

    let typing = false, idle;
    const setTyping = (v) => { if (v !== typing) { typing = v; room.updateState({ typing: v }); } };
    input.oninput = () => { setTyping(input.value !== ""); clearTimeout(idle); idle = setTimeout(() => setTyping(false), 3000); };

    document.querySelector("#send").onsubmit = async (e) => {
      e.preventDefault();
      const text = input.value.trim();
      if (!text) return;
      input.value = "";
      setTyping(false);
      await messages.create({ text, name: me.name });
    };
  </script>
</body>
</html>
```

---

## 5. AI summarizer (streaming)

Paste text, pick a style, and watch the summary stream in. Handles the "AI not configured"
and rate-limit cases.

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Summarizer</title>
  <meta name="description" content="Paste a doc or thread, get a summary">
  <style>
    :root { color-scheme: light dark; font: 16px/1.55 system-ui, sans-serif; --accent: #2a78d6; --line: #8884; }
    body { max-width: 760px; margin: 2rem auto; padding: 0 1rem; }
    textarea, select, button { font: inherit; }
    textarea { width: 100%; box-sizing: border-box; padding: .75rem; border-radius: 10px; }
    .bar { display: flex; gap: .5rem; margin: .5rem 0 1rem; }
    #out { white-space: pre-wrap; min-height: 3em; padding: 1rem; border: 1px solid var(--line); border-radius: 10px; }
    #out:empty::before { content: "The summary appears here."; opacity: .5; }
    #meta { font-size: .8em; opacity: .65; }
  </style>
</head>
<body>
  <h1>Summarizer</h1>
  <textarea id="text" rows="12" placeholder="Paste a document, Slack thread or meeting transcript…"></textarea>
  <div class="bar">
    <select id="style">
      <option value="Summarize as 3-7 concise bullet points starting with '- '.">Key points</option>
      <option value="Summarize in at most two sentences.">TL;DR</option>
      <option value="List the action items as '- owner: task (due date)'. Say 'No action items.' if there are none.">Action items</option>
    </select>
    <button id="go">Summarize</button>
  </div>
  <div id="out"></div>
  <p id="meta"></p>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const $ = (s) => document.querySelector(s);
    $("#go").onclick = async () => {
      const text = $("#text").value.trim();
      if (!text) return $("#text").focus();
      $("#go").disabled = true;
      $("#out").textContent = "";
      $("#meta").textContent = "Thinking…";
      try {
        const stream = backyard.ai.stream([{ role: "user", content: text }], {
          system: "You summarize internal documents for busy colleagues. " + $("#style").value + " Plain text only.",
          maxTokens: 800,
          temperature: 0.2,
          onText: (t) => ($("#out").textContent += t),
        });
        const res = await stream.done;
        $("#meta").textContent = `${res.model} · ${res.usage.input_tokens} tokens in, ${res.usage.output_tokens} out`;
      } catch (err) {
        $("#meta").textContent = "";
        $("#out").textContent =
          err.code === "unsupported" ? "AI is not enabled on this Backyard server." :
          err.code === "rate_limited" ? "Too many requests — try again in a minute." :
          err.code === "too_large" ? "That text is too long — try a shorter excerpt." :
          `Something went wrong: ${err.message}`;
      } finally {
        $("#go").disabled = false;
      }
    };
  </script>
</body>
</html>
```

---

## 6. Image generator gallery

Generate images from prompts; every result is saved (the image as a site file, its prompt in
the db) and appears in a shared gallery for everyone.

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Image Studio</title>
  <style>
    :root { color-scheme: light dark; font: 15px/1.45 system-ui, sans-serif; --line: #8884; }
    body { max-width: 1000px; margin: 2rem auto; padding: 0 1rem; }
    form { display: flex; gap: .5rem; } input { flex: 1; font: inherit; padding: .6rem; } button { font: inherit; }
    #status { min-height: 1.4em; opacity: .75; }
    #gallery { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 1rem; }
    figure { margin: 0; border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
    figure img { display: block; width: 100%; aspect-ratio: 1; object-fit: cover; background: #8882; }
    figcaption { padding: .5rem .75rem; font-size: .85em; }
    figcaption small { display: block; opacity: .6; }
  </style>
</head>
<body>
  <h1>Image Studio</h1>
  <form id="gen"><input id="prompt" placeholder="A cozy reading nook in a treehouse, watercolor" required maxlength="500"><button>Generate</button></form>
  <p id="status"></p>
  <section id="gallery"></section>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const images = backyard.db.collection("images");
    const me = await backyard.me();
    const status = document.querySelector("#status");

    images.watch((docs) => {
      document.querySelector("#gallery").replaceChildren(...docs.map((d) => {
        const fig = document.createElement("figure");
        fig.innerHTML = `<a target="_blank"><img loading="lazy" alt=""></a><figcaption><span></span><small></small></figcaption>`;
        fig.querySelector("a").href = d.url;
        fig.querySelector("img").src = d.url;
        fig.querySelector("img").alt = d.prompt;
        fig.querySelector("span").textContent = d.prompt;
        fig.querySelector("small").textContent = `${d.by} · ${new Date(d._meta.created_at).toLocaleDateString()}`;
        return fig;
      }));
    }, images.orderBy("_meta.created_at", "desc").limit(60));

    document.querySelector("#gen").onsubmit = async (e) => {
      e.preventDefault();
      const button = e.target.querySelector("button");
      const prompt = document.querySelector("#prompt").value.trim();
      button.disabled = true;
      status.textContent = "Painting… (this can take a few seconds)";
      try {
        const img = await backyard.ai.image(prompt, { size: "1024x1024" });
        await images.create({ prompt, url: img.url, by: me.name });
        status.textContent = "";
        document.querySelector("#prompt").value = "";
      } catch (err) {
        status.textContent = err.code === "unsupported" ? "Image generation is not enabled on this server." : `Failed: ${err.message}`;
      } finally {
        button.disabled = false;
      }
    };
  </script>
</body>
</html>
```

---

## 7. File drop and gallery

Drag files onto the page (or pick/paste them). Each upload is mirrored as a db document so the
gallery updates live for everyone; deleting removes both the file and the document.

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>File Drop</title>
  <style>
    :root { color-scheme: light dark; font: 15px/1.45 system-ui, sans-serif; --accent: #2a78d6; --line: #8884; }
    body { max-width: 960px; margin: 2rem auto; padding: 0 1rem; }
    #zone { display: block; padding: 2.5rem; text-align: center; border: 2px dashed var(--line); border-radius: 14px; cursor: pointer; }
    #zone.over { border-color: var(--accent); background: #2a78d611; }
    #zone input { display: none; }
    #status { min-height: 1.4em; opacity: .75; }
    #grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: .75rem; }
    .card { border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
    .thumb { display: grid; place-items: center; aspect-ratio: 4/3; background: #8882; font-size: 2rem; }
    .thumb img { width: 100%; height: 100%; object-fit: cover; }
    .info { display: flex; align-items: center; gap: .5rem; padding: .5rem .6rem; font-size: .85em; }
    .info a { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .info button { border: 0; background: none; cursor: pointer; font-size: 1rem; }
  </style>
</head>
<body>
  <h1>File Drop</h1>
  <label id="zone">Drop files here, paste, or <u>browse</u><input id="pick" type="file" multiple></label>
  <p id="status"></p>
  <section id="grid"></section>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const drops = backyard.db.collection("drops");
    const me = await backyard.me();
    const zone = document.querySelector("#zone");
    const status = document.querySelector("#status");
    const human = (n) => n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1048576).toFixed(1)} MB`;

    drops.watch((docs) => {
      document.querySelector("#grid").replaceChildren(...docs.map((d) => {
        const card = document.createElement("div");
        card.className = "card";
        card.innerHTML = `<div class="thumb"></div><div class="info"><a download></a><button title="Delete">🗑</button></div>`;
        const thumb = card.querySelector(".thumb");
        if (d.content_type.startsWith("image/")) {
          const img = new Image(); img.loading = "lazy"; img.src = d.url; img.alt = d.name; thumb.append(img);
        } else thumb.textContent = "📄";
        const a = card.querySelector("a");
        a.href = d.url; a.textContent = d.name; a.title = `${d.name} · ${human(d.size)} · ${d.by}`;
        card.querySelector("button").onclick = async () => {
          if (!confirm(`Delete ${d.name}?`)) return;
          await backyard.files.delete(d.file_id);
          await drops.delete(d.id);
        };
        return card;
      }));
    }, drops.orderBy("_meta.created_at", "desc").limit(200));

    async function upload(files) {
      let done = 0;
      for (const file of files) {
        status.textContent = `Uploading ${file.name} (${++done}/${files.length})…`;
        try {
          const info = await backyard.files.upload(file);
          await drops.create({ file_id: info.id, name: info.name, url: info.url, size: info.size, content_type: info.content_type, by: me.name });
        } catch (err) {
          status.textContent = err.code === "too_large" ? `${file.name} is too large.` : `${file.name}: ${err.message}`;
          return;
        }
      }
      status.textContent = `Uploaded ${files.length} file${files.length === 1 ? "" : "s"}.`;
    }

    document.querySelector("#pick").onchange = (e) => upload([...e.target.files]);
    zone.ondragover = (e) => { e.preventDefault(); zone.classList.add("over"); };
    zone.ondragleave = () => zone.classList.remove("over");
    zone.ondrop = (e) => { e.preventDefault(); zone.classList.remove("over"); upload([...e.dataTransfer.files]); };
    addEventListener("paste", (e) => { const files = [...e.clipboardData.files]; if (files.length) upload(files); });
  </script>
</body>
</html>
```

---

## 8. Kanban board

Three columns, drag and drop to move cards, live for everyone. A card is
`{title, status, order}`; moving a card sets its status and a new `order` (a timestamp, so it
lands at the bottom).

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Team Board</title>
  <style>
    :root { color-scheme: light dark; font: 15px/1.4 system-ui, sans-serif; --accent: #2a78d6; --line: #8884; }
    body { margin: 2rem auto; max-width: 1100px; padding: 0 1rem; }
    form { display: flex; gap: .5rem; margin-bottom: 1rem; } input { flex: 1; font: inherit; padding: .5rem; } button { font: inherit; }
    #board { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 1rem; }
    .col { background: #8881; border-radius: 12px; padding: .75rem; min-height: 200px; }
    .col.over { outline: 2px solid var(--accent); }
    .col h2 { font-size: .9rem; text-transform: uppercase; letter-spacing: .05em; opacity: .7; margin: 0 0 .5rem; }
    .card { display: flex; gap: .5rem; align-items: start; background: Canvas; border: 1px solid var(--line); border-radius: 8px;
            padding: .5rem .6rem; margin: .4rem 0; cursor: grab; }
    .card span { flex: 1; } .card small { display: block; opacity: .6; }
    .card button { border: 0; background: none; cursor: pointer; opacity: .5; }
  </style>
</head>
<body>
  <h1>Team Board</h1>
  <form id="add"><input id="title" placeholder="New card…" required maxlength="200"><button>Add</button></form>
  <section id="board"></section>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const cards = backyard.db.collection("cards");
    const me = await backyard.me();
    const COLUMNS = [["todo", "To do"], ["doing", "Doing"], ["done", "Done"]];
    const board = document.querySelector("#board");

    const cols = new Map(COLUMNS.map(([status, label]) => {
      const col = document.createElement("div");
      col.className = "col";
      col.innerHTML = `<h2></h2><div class="list"></div>`;
      col.querySelector("h2").textContent = label;
      col.ondragover = (e) => { e.preventDefault(); col.classList.add("over"); };
      col.ondragleave = () => col.classList.remove("over");
      col.ondrop = (e) => {
        e.preventDefault(); col.classList.remove("over");
        const id = e.dataTransfer.getData("text/plain");
        if (id) cards.update(id, { status, order: Date.now() });
      };
      board.append(col);
      return [status, col];
    }));

    cards.watch((docs) => {
      for (const [status, col] of cols) {
        const mine = docs.filter((c) => c.status === status);
        col.querySelector("h2").textContent = `${COLUMNS.find((c) => c[0] === status)[1]} · ${mine.length}`;
        col.querySelector(".list").replaceChildren(...mine.map((c) => {
          const el = document.createElement("div");
          el.className = "card";
          el.draggable = true;
          el.innerHTML = `<span></span><button title="Delete">✕</button>`;
          el.querySelector("span").textContent = c.title;
          const by = document.createElement("small");
          by.textContent = c.by ?? "";
          el.querySelector("span").append(by);
          el.ondragstart = (e) => e.dataTransfer.setData("text/plain", c.id);
          el.querySelector("button").onclick = () => cards.delete(c.id);
          return el;
        }));
      }
    }, cards.orderBy("order").limit(1000));

    document.querySelector("#add").onsubmit = async (e) => {
      e.preventDefault();
      const input = document.querySelector("#title");
      const title = input.value.trim();
      if (!title) return;
      input.value = "";
      await cards.create({ title, status: "todo", order: Date.now(), by: me.name });
    };
  </script>
</body>
</html>
```

---

## 9. Warehouse KPI dashboard

Stat tiles plus an SVG line chart (with hover readout) from a **named query**. When the
warehouse is not available (`unsupported`, `forbidden`, `not_found`…) it falls back to clearly
labeled demo data. An admin defines the query in `backyard.yaml`:

```yaml
warehouse:
  sources:
    default: { type: postgres, dsn: "${ANALYTICS_DSN}" }
  queries:
    daily_signups:
      description: New signups per day
      params: [days]
      sql: SELECT day, signups FROM analytics.daily_signups WHERE day >= CURRENT_DATE - CAST(@days AS integer) ORDER BY day
```

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Signups KPI</title>
  <style>
    :root { color-scheme: light dark; font: 15px/1.45 system-ui, sans-serif;
            --surface: #fcfcfb; --ink: #0b0b0b; --ink-2: #52514e; --muted: #898781; --grid: #e1e0d9; --series: #2a78d6; }
    @media (prefers-color-scheme: dark) {
      :root { --surface: #1a1a19; --ink: #f0efec; --ink-2: #c3c2b7; --grid: #2c2c2a; --series: #3987e5; }
    }
    body { max-width: 900px; margin: 2rem auto; padding: 0 1rem; background: var(--surface); color: var(--ink); }
    .note { padding: .6rem .9rem; border-radius: 8px; background: #eda10022; color: var(--ink-2); }
    .tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 1rem; margin: 1rem 0; }
    .tile .label { color: var(--ink-2); font-size: .85em; } .tile .value { font-size: 2rem; font-weight: 600; }
    #chart { position: relative; } svg { display: block; width: 100%; height: auto; }
    #tip { position: absolute; pointer-events: none; padding: .3rem .5rem; border-radius: 6px; background: var(--ink); color: var(--surface);
           font-size: .8em; transform: translate(-50%, -110%); white-space: nowrap; }
  </style>
</head>
<body>
  <h1>Daily signups</h1>
  <p id="note" class="note" hidden></p>
  <div class="tiles">
    <div class="tile"><div class="label">Last 30 days</div><div class="value" id="total">–</div></div>
    <div class="tile"><div class="label">Daily average</div><div class="value" id="avg">–</div></div>
    <div class="tile"><div class="label">Last 7 vs prior 7 days</div><div class="value" id="delta">–</div></div>
  </div>
  <div id="chart"><svg id="svg" viewBox="0 0 800 300" role="img" aria-label="Daily signups, last 30 days"></svg><div id="tip" hidden></div></div>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    async function load() {
      try {
        const r = await backyard.warehouse.named("daily_signups", { days: 30 });
        return { rows: r.rows.map((x) => ({ day: String(x.day).slice(0, 10), value: Number(x.signups) })) };
      } catch (err) {
        const today = new Date();
        const rows = Array.from({ length: 30 }, (_, i) => {
          const d = new Date(today); d.setDate(d.getDate() - 29 + i);
          return { day: d.toLocaleDateString("en-CA"), value: Math.round(120 + i * 3 + 25 * Math.sin(i / 2.2) + (d.getDay() % 6 === 0 ? -40 : 0)) };
        });
        return { rows, demo: `Showing demo data — the warehouse answered "${err.code}". Ask an admin to add the daily_signups named query.` };
      }
    }

    const { rows, demo } = await load();
    if (demo) Object.assign(document.querySelector("#note"), { hidden: false, textContent: demo });
    if (rows.length) draw(rows);
    else document.querySelector("#chart").textContent = "No data for this period.";

    function draw(rows) {
      const fmt = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1, notation: "compact" });
      const sum = (a) => a.reduce((s, r) => s + r.value, 0);
      const last7 = sum(rows.slice(-7)), prev7 = sum(rows.slice(-14, -7));
      document.querySelector("#total").textContent = fmt.format(sum(rows));
      document.querySelector("#avg").textContent = fmt.format(sum(rows) / (rows.length || 1));
      document.querySelector("#delta").textContent = prev7 ? `${last7 >= prev7 ? "▲" : "▼"} ${Math.abs(((last7 - prev7) / prev7) * 100).toFixed(1)}%` : "–";

      // Chart: 2px line, light area wash, hairline grid, crosshair + tooltip.
      const W = 800, H = 300, L = 44, R = 16, T = 16, B = 28;
      // Round y ticks: a "nice" step (1, 2, 2.5 or 5 × 10ⁿ) about a quarter of the maximum.
      const peak = Math.max(1, ...rows.map((r) => r.value));
      const pow = 10 ** Math.floor(Math.log10(peak / 4));
      const step = [1, 2, 2.5, 5, 10].map((m) => m * pow).find((s) => s >= peak / 4);
      const max = step * Math.ceil(peak / step);
      const ticks = Array.from({ length: Math.round(max / step) + 1 }, (_, i) => i * step);
      const x = (i) => L + (i / Math.max(1, rows.length - 1)) * (W - L - R);
      const y = (v) => H - B - (v / max) * (H - T - B);
      const line = rows.map((r, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(r.value).toFixed(1)}`).join("");
      const svg = document.querySelector("#svg");
      svg.innerHTML = `
        ${ticks.map((t) => `<line x1="${L}" x2="${W - R}" y1="${y(t)}" y2="${y(t)}" stroke="var(--grid)"/>
          <text x="${L - 8}" y="${y(t) + 4}" text-anchor="end" font-size="11" fill="var(--muted)">${fmt.format(t)}</text>`).join("")}
        ${rows.map((r, i) => i % 7 === 0 ? `<text x="${x(i)}" y="${H - 8}" text-anchor="middle" font-size="11" fill="var(--muted)">${r.day.slice(5)}</text>` : "").join("")}
        <path d="${line}L${x(rows.length - 1)},${y(0)}L${x(0)},${y(0)}Z" fill="var(--series)" opacity=".1"/>
        <path d="${line}" fill="none" stroke="var(--series)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>
        <line id="cross" y1="${T}" y2="${H - B}" stroke="var(--muted)" visibility="hidden"/>
        <circle id="dot" r="4" fill="var(--series)" stroke="var(--surface)" stroke-width="2" visibility="hidden"/>`;
      const tip = document.querySelector("#tip"), cross = svg.querySelector("#cross"), dot = svg.querySelector("#dot");
      svg.onpointermove = (e) => {
        const box = svg.getBoundingClientRect();
        const px = ((e.clientX - box.left) / box.width) * W;
        const i = Math.max(0, Math.min(rows.length - 1, Math.round(((px - L) / (W - L - R)) * (rows.length - 1))));
        for (const el of [cross, dot]) el.setAttribute("visibility", "visible");
        cross.setAttribute("x1", x(i)); cross.setAttribute("x2", x(i));
        dot.setAttribute("cx", x(i)); dot.setAttribute("cy", y(rows[i].value));
        tip.hidden = false;
        tip.textContent = `${rows[i].value.toLocaleString()} signups · ${rows[i].day}`;
        tip.style.left = `${(x(i) / W) * 100}%`; tip.style.top = `${(y(rows[i].value) / H) * 100}%`;
      };
      svg.onpointerleave = () => { tip.hidden = true; for (const el of [cross, dot]) el.setAttribute("visibility", "hidden"); };
    }
  </script>
</body>
</html>
```

---

## 10. Guestbook

The smallest useful realtime app: sign with one click (your name comes from the company
login), newest first, delete your own entries.

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Guestbook</title>
  <style>
    :root { color-scheme: light dark; font: 16px/1.5 system-ui, sans-serif; --line: #8884; }
    body { max-width: 600px; margin: 2rem auto; padding: 0 1rem; }
    form { display: flex; gap: .5rem; } input { flex: 1; font: inherit; padding: .5rem; } button { font: inherit; }
    ol { list-style: none; padding: 0; }
    li { display: flex; gap: .75rem; padding: .75rem 0; border-bottom: 1px solid var(--line); }
    .avatar { width: 36px; height: 36px; border-radius: 50%; flex: none; display: grid; place-items: center; background: #2a78d6; color: #fff; object-fit: cover; }
    li div { flex: 1; } small { opacity: .6; } li button { border: 0; background: none; cursor: pointer; opacity: .5; }
  </style>
</head>
<body>
  <h1>Guestbook</h1>
  <form id="sign"><input id="message" placeholder="Say hi…" required maxlength="280"><button>Sign</button></form>
  <ol id="entries"></ol>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const entries = backyard.db.collection("entries");
    const me = await backyard.me();

    function avatar(e) {
      if (e.avatar_url) return Object.assign(new Image(), { src: e.avatar_url, alt: "", className: "avatar" });
      const span = document.createElement("span");
      span.className = "avatar";
      span.textContent = (e.name || "?").split(/\s+/).map((w) => w[0]).slice(0, 2).join("").toUpperCase();
      return span;
    }

    entries.watch((docs) => {
      document.querySelector("#entries").replaceChildren(...docs.map((e) => {
        const li = document.createElement("li");
        const body = document.createElement("div");
        body.innerHTML = `<b></b> <small></small><div></div>`;
        body.querySelector("b").textContent = e.name;
        body.querySelector("small").textContent = new Date(e._meta.created_at).toLocaleString();
        body.querySelector("div").textContent = e.message;
        li.append(avatar(e), body);
        if (e._meta.created_by === me.id) {
          const del = Object.assign(document.createElement("button"), { textContent: "✕", title: "Delete" });
          del.onclick = () => entries.delete(e.id);
          li.append(del);
        }
        return li;
      }));
    }, entries.orderBy("_meta.created_at", "desc").limit(100));

    document.querySelector("#sign").onsubmit = async (ev) => {
      ev.preventDefault();
      const input = document.querySelector("#message");
      const message = input.value.trim();
      if (!message) return;
      input.value = "";
      await entries.create({ message, name: me.name, avatar_url: me.avatar_url ?? null });
    };
  </script>
</body>
</html>
```

---

## 11. Game leaderboard

A five-second click-frenzy game with a shared top-10. Each player has one document (their
best score), so the leaderboard is a simple sorted query; a room shows who is playing now.

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Click Frenzy</title>
  <style>
    :root { color-scheme: light dark; font: 16px/1.5 system-ui, sans-serif; --accent: #2a78d6; --line: #8884; }
    body { max-width: 760px; margin: 2rem auto; padding: 0 1rem; display: grid; grid-template-columns: 1fr 260px; gap: 2rem; }
    @media (max-width: 640px) { body { grid-template-columns: 1fr; } }
    #pad { width: 100%; aspect-ratio: 1; border: 0; border-radius: 24px; font: 700 2.5rem system-ui; color: #fff; background: var(--accent); cursor: pointer; user-select: none; }
    #pad:active { transform: scale(.98); }
    ol { padding-left: 1.4rem; } li { padding: .2rem 0; } li.me { font-weight: 700; }
    li span { float: right; font-variant-numeric: tabular-nums; }
    #online { opacity: .7; font-size: .9em; }
  </style>
</head>
<body>
  <main>
    <h1>Click Frenzy</h1>
    <p>Click as fast as you can for 5 seconds.</p>
    <button id="pad">Start</button>
    <p id="result"></p>
  </main>
  <aside>
    <h2>Top 10</h2>
    <ol id="board"></ol>
    <p id="online"></p>
  </aside>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const scores = backyard.db.collection("scores");
    const me = await backyard.me();
    const myId = me.id.replace(/[^A-Za-z0-9_\-:.]/g, "_").slice(0, 120);
    const pad = document.querySelector("#pad");
    const room = backyard.socket.room("arcade", { state: { playing: false } });

    scores.watch((docs) => {
      document.querySelector("#board").replaceChildren(...docs.map((s) => {
        const li = document.createElement("li");
        li.className = s.id === myId ? "me" : "";
        li.textContent = s.name;
        const n = document.createElement("span");
        n.textContent = s.best;
        li.append(n);
        return li;
      }));
    }, scores.orderBy("best", "desc").limit(10));

    room.on("members", (members) => {
      const playing = members.filter((m) => m.state.playing).map((m) => m.user.name);
      document.querySelector("#online").textContent =
        `${members.length + 1} here` + (playing.length ? ` · playing: ${[...new Set(playing)].join(", ")}` : "");
    });

    let clicks = 0, running = false;
    pad.onclick = () => {
      if (running) { pad.textContent = ++clicks; return; }
      running = true; clicks = 0; pad.textContent = "0";
      room.updateState({ playing: true });
      setTimeout(finish, 5000);
    };

    async function finish() {
      running = false;
      room.updateState({ playing: false });
      pad.disabled = true;
      setTimeout(() => { pad.disabled = false; pad.textContent = "Again"; }, 1200);
      const current = await scores.get(myId);                // one doc per player: keep the best
      const best = Math.max(clicks, current?.best ?? 0);
      await scores.set(myId, { name: me.name, best, plays: (current?.plays ?? 0) + 1, last: clicks });
      document.querySelector("#result").textContent =
        clicks > (current?.best ?? 0) ? `New personal best: ${clicks}!` : `${clicks} clicks — your best is ${best}.`;
    }
  </script>
</body>
</html>
```

---

## 12. Sharing code between sites

Sites are plain files on internal URLs, so one site can be a shared library for others. Deploy
a folder `ui-kit/` containing `kit.js` and `kit.css`, then include it from any site by
**absolute** URL:

```html
<!-- host-mode servers -->
<link rel="stylesheet" href="https://ui-kit.backyard.example.com/kit.css">
<script src="https://ui-kit.backyard.example.com/kit.js"></script>
<!-- path-mode servers -->
<script src="https://backyard.example.com/s/ui-kit/kit.js"></script>
```

```js
// ui-kit/kit.js — a tiny shared helper used by many sites
window.kit = {
  // Renders the signed-in user's avatar chip into el. backyard.* calls act on the
  // INCLUDING site (the page's site), not on ui-kit.
  async userChip(el) {
    const me = await backyard.me();
    el.textContent = me.name;
    el.title = me.email ?? "";
  },
  key: (s) => s.replace(/[^A-Za-z0-9_\-:.]/g, "_").slice(0, 128),
};
```

- Load the Backyard SDK in the including page before the shared script.
- Data APIs (`db`, `files`, `socket`…) always use the site the **page** belongs to. To read
  another site's data, embed that site in an `<iframe>` or link to it — sites cannot read each
  other's databases.
- Deploys overwrite the library in place: version breaking changes (`kit.v2.js`) so existing
  sites keep working.
- Find reusable code with `backyard sites list`, then `backyard pull <site>` (or the MCP tools
  `backyard_site_files` / `backyard_read_site_file`).
