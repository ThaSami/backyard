// Live Poll: the host (this page with ?host) asks a question, the audience
// (the plain URL) answers on their phones, results update live for everyone.
//
// Data:
//   poll     one document "current": {question, options: string[], round, open, host}
//   answers  {round, choice, name}  id = "<round>.<user>" → one answer per person per round
// Presence: room "poll" counts the people watching.

// The SDK is loaded by index.html; without it (e.g. served by a plain static file server) say so.
if (!window.backyard) {
  document.querySelector("#audience").hidden = false;
  document.querySelector("#a-question").textContent = "Could not load the poll";
  document.querySelector("#a-hint").textContent = "Open this page through Backyard (backyard dev or a deployed site).";
  throw new Error("The Backyard SDK (/__backyard/backyard.js) is not loaded");
}

const polls = backyard.db.collection("poll");
const answers = backyard.db.collection("answers");
const isHost = new URLSearchParams(location.search).has("host");

const state = { me: null, poll: null, answers: [], people: 1, stopAnswers: null };
const $ = (sel) => document.querySelector(sel);
const key = (s) => String(s).replace(/[^A-Za-z0-9_\-:.]/g, "_").slice(0, 120);
const myAnswerId = () => `${state.poll.round}.${key(state.me.id)}`;

function toast(message) {
  const el = $("#toast");
  el.textContent = message;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (el.hidden = true), 3500);
}

const fail = (err) => toast(err.code === "rate_limited" ? "Too many clicks, slow down a little." : `Something went wrong: ${err.message}`);

// ---------------------------------------------------------------------------
// Results bars (shared by both views). Bars are reused between renders so
// their widths animate smoothly.

function renderResults(container) {
  const { poll } = state;
  const counts = poll.options.map((_, i) => state.answers.filter((a) => a.choice === i).length);
  const total = state.answers.length;
  const max = Math.max(...counts);
  if (container.dataset.round !== String(poll.round) || container.children.length !== poll.options.length + 1) {
    container.dataset.round = String(poll.round);
    container.replaceChildren(
      ...poll.options.map(() => {
        const row = document.createElement("div");
        row.className = "result";
        row.innerHTML = `<div class="label"><span class="opt"></span><span class="num"></span></div><div class="track"><div class="fill"></div></div>`;
        return row;
      }),
      Object.assign(document.createElement("p"), { className: "total muted" }),
    );
  }
  poll.options.forEach((opt, i) => {
    const row = container.children[i];
    const pct = total ? Math.round((counts[i] / total) * 100) : 0;
    row.classList.toggle("top", total > 0 && counts[i] === max);
    row.querySelector(".opt").textContent = opt;
    row.querySelector(".num").textContent = `${pct}% · ${counts[i]}`;
    row.querySelector(".fill").style.width = `${total ? (counts[i] / total) * 100 : 0}%`;
  });
  container.lastElementChild.textContent = `${total} answer${total === 1 ? "" : "s"}`;
}

// ---------------------------------------------------------------------------
// Audience

function renderAudience() {
  const { poll } = state;
  $("#audience").hidden = false;
  if (!poll) {
    $("#a-question").textContent = "Waiting for the host…";
    $("#a-hint").textContent = "The question appears here as soon as the host starts it.";
    $("#a-options").replaceChildren();
    $("#a-results").hidden = true;
    $("#a-live").hidden = true;
    return;
  }
  const mine = state.answers.find((a) => a.id === myAnswerId())?.choice;
  $("#a-live").hidden = !poll.open;
  $("#a-question").textContent = poll.question;
  $("#a-hint").textContent = !poll.open
    ? "Voting is closed. Here is how the room answered."
    : mine === undefined
      ? "Tap an answer. You can change it until voting closes."
      : "Thanks! You can still change your answer.";
  $("#a-options").replaceChildren(
    ...poll.options.map((opt, i) => {
      const b = Object.assign(document.createElement("button"), { className: "option", textContent: opt, disabled: !poll.open });
      b.classList.toggle("chosen", mine === i);
      b.setAttribute("aria-pressed", String(mine === i));
      b.onclick = () => answers.set(myAnswerId(), { round: poll.round, choice: i, name: state.me.name }).catch(fail);
      return b;
    }),
  );
  // Show results once you answered (or when voting is closed) so early votes are not biased.
  const show = mine !== undefined || !poll.open;
  $("#a-results").hidden = !show;
  if (show) renderResults($("#a-results"));
}

// ---------------------------------------------------------------------------
// Host

function optionRow(value = "") {
  const row = document.createElement("div");
  row.className = "option-row";
  row.innerHTML = `<input maxlength="80" placeholder="Option"><button type="button" class="icon" title="Remove option" aria-label="Remove option">✕</button>`;
  row.querySelector("input").value = value;
  row.querySelector("button").onclick = () => {
    if ($("#h-options").children.length > 2) row.remove();
  };
  return row;
}

function setupHost() {
  $("#host").hidden = false;
  const audienceURL = location.origin + location.pathname;
  $("#h-link").textContent = audienceURL;
  $("#h-copy").onclick = () =>
    navigator.clipboard.writeText(audienceURL).then(() => toast("Link copied"), () => toast(audienceURL));
  for (const v of ["", "", ""]) $("#h-options").append(optionRow(v));
  $("#h-add-option").onclick = () => {
    if ($("#h-options").children.length < 8) $("#h-options").append(optionRow());
  };
  $("#h-form").onsubmit = async (e) => {
    e.preventDefault();
    const question = $("#h-question").value.trim();
    const options = [...new Set([...$("#h-options").querySelectorAll("input")].map((i) => i.value.trim()).filter(Boolean))];
    if (options.length < 2) return toast("Give at least two different options.");
    try {
      await polls.set("current", { question, options, round: (state.poll?.round ?? 0) + 1, open: true, host: state.me.name });
      toast("New round started");
    } catch (err) {
      fail(err);
    }
  };
  $("#h-toggle").onclick = () => polls.update("current", { open: !state.poll.open }).catch(fail);
}

function renderHost() {
  const { poll } = state;
  $("#h-people").textContent = `${state.people} watching`;
  $("#h-controls").hidden = !poll;
  if (!poll) return;
  $("#h-status").textContent = `Round ${poll.round} · ${poll.open ? "voting open" : "voting closed"}`;
  $("#h-status").classList.toggle("open", poll.open);
  $("#h-question-live").textContent = poll.question;
  $("#h-toggle").textContent = poll.open ? "Close voting" : "Reopen voting";
  renderResults($("#h-results"));
}

const render = () => (isHost ? renderHost() : renderAudience());

// ---------------------------------------------------------------------------
// Startup

async function main() {
  state.me = await backyard.me();
  if (isHost) setupHost();

  polls.watch(
    ([doc]) => {
      const newRound = doc?.round !== state.poll?.round;
      state.poll = doc ?? null;
      if (newRound) {
        // Re-target the answers live query at the new round.
        state.stopAnswers?.();
        state.answers = [];
        state.stopAnswers = state.poll
          ? answers.watch(
              (docs) => {
                state.answers = docs;
                render();
              },
              answers.where("round", "==", state.poll.round).limit(1000),
            )
          : null;
      }
      render();
    },
    polls.where("id", "==", "current"),
  );

  const room = backyard.socket.room("poll");
  room.on("members", (members) => {
    state.people = new Set([state.me.id, ...members.map((m) => m.user.id)]).size;
    if (isHost) renderHost();
  });
}

main().catch((err) => {
  $("#audience").hidden = false;
  $("#a-question").textContent = "Could not load the poll";
  $("#a-hint").textContent = err.message;
});
