// AI Chat: streaming chat with a model picker.
//
// backyard.ai.models() lists the model aliases the admins configured;
// backyard.ai.stream() streams the answer. No API key ever reaches the
// browser: the Backyard server calls the provider. The conversation is kept
// in sessionStorage (this tab only) — nothing is stored on the server.

const STYLES = {
  "Helpful": "You are a helpful assistant for employees of our company. Be accurate and friendly.",
  "Concise": "You are a helpful assistant. Answer as briefly as possible; use bullet points when listing.",
  "Explain simply": "You explain things simply, as to a smart newcomer. Use short sentences and one concrete example.",
  "Writing editor": "You are an editor. Improve the user's text for clarity and tone, then list the main changes.",
};
const SUGGESTIONS = [
  "Draft a friendly announcement for our team offsite",
  "Explain websockets in three sentences",
  "Give me 5 ideas for an internal hack week project",
  "Turn these notes into action items: …",
];
const STORE_KEY = "ai-chat.history";

const $ = (sel) => document.querySelector(sel);
const state = { me: null, history: loadHistory(), busy: false };

// ---------------------------------------------------------------------------
// Persistence (per tab; storage may be unavailable)

function loadHistory() {
  try {
    const v = JSON.parse(sessionStorage.getItem(STORE_KEY) ?? "[]");
    return Array.isArray(v) ? v.filter((m) => m && typeof m.content === "string" && (m.role === "user" || m.role === "assistant")) : [];
  } catch {
    return [];
  }
}

function saveHistory() {
  try {
    sessionStorage.setItem(STORE_KEY, JSON.stringify(state.history));
  } catch {
    // ignore: private mode or quota
  }
}

function pref(name, value) {
  try {
    if (value === undefined) return localStorage.getItem(`ai-chat.${name}`);
    localStorage.setItem(`ai-chat.${name}`, value);
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Safe Markdown subset → DOM (no innerHTML with model output)

function inline(el, text) {
  const re = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\(https?:\/\/[^)\s]+\))/g;
  let last = 0;
  for (const m of text.matchAll(re)) {
    if (m.index > last) el.append(text.slice(last, m.index));
    const tok = m[0];
    if (tok.startsWith("`")) {
      el.append(Object.assign(document.createElement("code"), { textContent: tok.slice(1, -1) }));
    } else if (tok.startsWith("**")) {
      el.append(Object.assign(document.createElement("strong"), { textContent: tok.slice(2, -2) }));
    } else {
      const [, label, href] = /^\[([^\]]+)\]\(([^)]+)\)$/.exec(tok);
      el.append(Object.assign(document.createElement("a"), { href, textContent: label, target: "_blank", rel: "noopener noreferrer" }));
    }
    last = m.index + tok.length;
  }
  if (last < text.length) el.append(text.slice(last));
}

const BLOCK = /^(```|\s*[-*] |\s*\d+[.)] |#{1,6} )/;

function markdown(text) {
  const frag = document.createDocumentFragment();
  const lines = text.split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.startsWith("```")) {
      const code = [];
      for (i++; i < lines.length && !lines[i].startsWith("```"); i++) code.push(lines[i]);
      i++; // closing fence (absent while still streaming)
      const pre = document.createElement("pre");
      pre.append(Object.assign(document.createElement("code"), { textContent: code.join("\n") }));
      const copy = Object.assign(document.createElement("button"), { className: "copy", textContent: "Copy", type: "button" });
      copy.onclick = () => navigator.clipboard.writeText(code.join("\n")).then(() => (copy.textContent = "Copied"));
      pre.append(copy);
      frag.append(pre);
    } else if (/^\s*[-*] /.test(line) || /^\s*\d+[.)] /.test(line)) {
      const ordered = /^\s*\d+[.)] /.test(line);
      const list = document.createElement(ordered ? "ol" : "ul");
      const item = ordered ? /^\s*\d+[.)] / : /^\s*[-*] /;
      for (; i < lines.length && item.test(lines[i]); i++) {
        const li = document.createElement("li");
        inline(li, lines[i].replace(item, ""));
        list.append(li);
      }
      frag.append(list);
    } else if (/^#{1,6} /.test(line)) {
      const h = document.createElement("h3");
      inline(h, line.replace(/^#{1,6} /, ""));
      frag.append(h);
      i++;
    } else if (!line.trim()) {
      i++;
    } else {
      const para = [];
      for (; i < lines.length && lines[i].trim() && !BLOCK.test(lines[i]); i++) para.push(lines[i]);
      const p = document.createElement("p");
      inline(p, para.join("\n"));
      frag.append(p);
    }
  }
  return frag;
}

// ---------------------------------------------------------------------------
// Rendering

function bubble(msg) {
  const el = document.createElement("article");
  el.className = `msg ${msg.role}`;
  const body = Object.assign(document.createElement("div"), { className: "body" });
  if (msg.role === "assistant") body.append(markdown(msg.content));
  else body.textContent = msg.content;
  el.append(body);
  if (msg.meta) el.append(Object.assign(document.createElement("div"), { className: "meta", textContent: msg.meta }));
  return el;
}

function renderAll() {
  $("#welcome").hidden = state.history.length > 0;
  $("#log").replaceChildren($("#welcome"), ...state.history.map(bubble));
  scrollDown(true);
}

function scrollDown(force) {
  const log = $("#log");
  if (force || log.scrollHeight - log.scrollTop - log.clientHeight < 120) log.scrollTop = log.scrollHeight;
}

function banner(text) {
  $("#banner").hidden = !text;
  $("#banner").textContent = text ?? "";
}

function setBusy(busy) {
  state.busy = busy;
  $("#send").disabled = busy;
  $("#composer").classList.toggle("busy", busy);
}

// ---------------------------------------------------------------------------
// Sending

function explain(err) {
  switch (err.code) {
    case "unsupported":
      return "AI is not enabled on this Backyard server. Ask an admin to configure a provider.";
    case "rate_limited":
      return "You are sending messages too quickly. Wait a minute and try again.";
    case "too_large":
    case "bad_request":
      return `The request was rejected: ${err.message}. Try a shorter message or start a new chat.`;
    case "upstream_error":
      return "The AI provider had a problem. Please try again.";
    default:
      return `Something went wrong: ${err.message}`;
  }
}

async function send(text) {
  if (state.busy || !text.trim()) return;
  banner("");
  state.history.push({ role: "user", content: text.trim() });
  saveHistory();
  renderAll();

  const answer = { role: "assistant", content: "" };
  const el = bubble(answer);
  el.classList.add("streaming");
  $("#log").append(el);
  const body = el.querySelector(".body");
  setBusy(true);

  let frame = 0;
  const paint = () => {
    frame = 0;
    body.replaceChildren(markdown(answer.content));
    scrollDown();
  };
  try {
    const stream = backyard.ai.stream(
      state.history.map(({ role, content }) => ({ role, content })),
      {
        model: $("#model").value || undefined,
        system: STYLES[$("#style").value] ?? STYLES.Helpful,
        maxTokens: 2048,
        onText: (t) => {
          answer.content += t;
          frame ||= requestAnimationFrame(paint);
        },
      },
    );
    const result = await stream.done;
    answer.content = result.content || answer.content;
    answer.meta = `${result.model} · ${result.usage.output_tokens} tokens${result.stop_reason === "max_tokens" ? " · cut off (length limit)" : ""}`;
    state.history.push(answer);
    saveHistory();
  } catch (err) {
    // Drop the failed turn so the user can retry it.
    const failed = state.history.pop();
    $("#input").value = failed?.content ?? "";
    banner(explain(err));
  } finally {
    cancelAnimationFrame(frame);
    setBusy(false);
    renderAll();
    $("#input").focus();
  }
}

// ---------------------------------------------------------------------------
// Startup

function autosize() {
  const t = $("#input");
  t.style.height = "auto";
  t.style.height = `${Math.min(t.scrollHeight, 240)}px`;
}

async function loadModels() {
  try {
    const { default: def, models } = await backyard.ai.models();
    const chat = models.filter((m) => m.kind === "chat");
    const list = chat.length ? chat : models;
    if (!list.length) throw Object.assign(new Error("no models"), { code: "unsupported" });
    // The remembered model if it still exists, else the server default.
    const saved = pref("model");
    const pick = list.some((m) => m.name === saved) ? saved : list.some((m) => m.name === def) ? def : list[0].name;
    $("#model").replaceChildren(
      ...list.map((m) =>
        Object.assign(document.createElement("option"), {
          value: m.name,
          textContent: m.description ? `${m.name} — ${m.description}` : m.name,
          selected: m.name === pick,
        }),
      ),
    );
    $("#model").disabled = false;
  } catch (err) {
    // An empty value: chat then uses the server's default model.
    $("#model").replaceChildren(Object.assign(document.createElement("option"), { value: "", textContent: "unavailable" }));
    banner(explain(err));
    if (err.code === "unsupported") {
      $("#input").disabled = true;
      $("#send").disabled = true;
    }
  }
}

async function main() {
  $("#style").append(...Object.keys(STYLES).map((s) => Object.assign(document.createElement("option"), { value: s, textContent: s })));
  $("#style").value = pref("style") ?? "Helpful";
  $("#style").onchange = () => pref("style", $("#style").value);
  $("#model").onchange = () => pref("model", $("#model").value);
  $("#suggestions").append(
    ...SUGGESTIONS.map((s) => {
      const b = Object.assign(document.createElement("button"), { className: "chip", textContent: s, type: "button" });
      b.onclick = () => {
        $("#input").value = s.endsWith("…") ? s.slice(0, -1) : "";
        if (s.endsWith("…")) $("#input").focus();
        else send(s);
      };
      return b;
    }),
  );
  $("#composer").onsubmit = (e) => {
    e.preventDefault();
    const text = $("#input").value;
    $("#input").value = "";
    autosize();
    send(text);
  };
  $("#input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      $("#composer").requestSubmit();
    }
  });
  $("#input").addEventListener("input", autosize);
  $("#new-chat").onclick = () => {
    if (state.busy) return;
    state.history = [];
    saveHistory();
    banner("");
    renderAll();
    $("#input").focus();
  };

  renderAll();
  await loadModels();
  state.me = await backyard.me();
  if (!state.me.anonymous) $("#hello").textContent = `What can I help with, ${state.me.name.split(" ")[0]}?`;
}

main().catch((err) => banner(window.backyard ? explain(err) : "Open this page through Backyard (backyard dev or a deployed site)."));
