// File Drop: uploads + live gallery + delete.
//
// backyard.files stores the bytes and lists them (newest first). Files have no
// realtime feed of their own, so every upload/delete also writes the single
// document signals/files; everyone subscribes to the "signals" collection and
// refreshes the list when it changes. Uploader names are resolved with
// backyard.id.lookup().

// The SDK is loaded by index.html; without it (e.g. served by a plain static file server) say so.
if (!window.backyard) {
  document.querySelector("#loading").textContent = "Open this page through Backyard (backyard dev or a deployed site).";
  throw new Error("The Backyard SDK (/__backyard/backyard.js) is not loaded");
}

const signals = backyard.db.collection("signals");
const PAGE = 200;

const $ = (sel) => document.querySelector(sel);
const state = { files: [], kind: "all", query: "", me: null };
const names = new Map(); // user id → display name (lookup cache)

// ---------------------------------------------------------------------------
// Helpers

function human(bytes) {
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  let n = bytes;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n < 10 && i ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}

function kindOf(f) {
  const t = f.content_type || "";
  if (t.startsWith("image/")) return "image";
  if (t.startsWith("video/")) return "video";
  if (/pdf|msword|officedocument|text\/|presentation|spreadsheet|csv|json|markdown/.test(t)) return "doc";
  return "other";
}

const ICONS = { image: "🖼️", video: "🎬", doc: "📄", other: "📦" };

function toast(text) {
  const el = $("#toast");
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (el.hidden = true), 4000);
}

async function nameOf(userId) {
  if (names.has(userId)) return names.get(userId);
  let name = userId.startsWith("anon:") ? "a guest" : userId;
  names.set(userId, name);
  if (userId.includes("@")) {
    try {
      const user = await backyard.id.lookup(userId);
      if (user?.name) name = user.name;
    } catch {
      // keep the id
    }
  }
  if (state.me && userId === state.me.id) name = "you";
  names.set(userId, name);
  return name;
}

// ---------------------------------------------------------------------------
// Gallery

function matches(f) {
  return (state.kind === "all" || kindOf(f) === state.kind) && f.name.toLowerCase().includes(state.query);
}

function card(f) {
  const el = document.createElement("article");
  el.className = "card";
  const kind = kindOf(f);
  const thumb = document.createElement(kind === "image" ? "button" : "a");
  thumb.className = "thumb";
  if (kind === "image") {
    thumb.type = "button";
    thumb.title = `View ${f.name}`;
    thumb.append(Object.assign(new Image(), { src: f.url, alt: f.name, loading: "lazy", decoding: "async" }));
    thumb.onclick = () => openViewer(f);
  } else if (kind === "video") {
    thumb.href = f.url;
    thumb.target = "_blank";
    const v = Object.assign(document.createElement("video"), { src: `${f.url}#t=0.5`, preload: "metadata", muted: true });
    thumb.append(v);
  } else {
    thumb.href = f.url;
    thumb.target = "_blank";
    thumb.append(Object.assign(document.createElement("span"), { className: "icon", textContent: ICONS[kind] }));
  }

  const info = document.createElement("div");
  info.className = "info";
  info.innerHTML = `<a class="name" download></a><span class="meta"></span>`;
  const link = info.querySelector(".name");
  link.href = f.url;
  link.textContent = f.name;
  link.title = `Download ${f.name}`;
  const meta = info.querySelector(".meta");
  meta.textContent = `${human(f.size)} · ${new Date(f.created_at).toLocaleDateString()}`;
  nameOf(f.created_by).then((n) => (meta.textContent = `${human(f.size)} · ${n} · ${new Date(f.created_at).toLocaleDateString()}`));

  const del = Object.assign(document.createElement("button"), { className: "delete", textContent: "🗑", title: `Delete ${f.name}` });
  del.setAttribute("aria-label", `Delete ${f.name}`);
  del.onclick = () => remove(f);
  info.append(del);

  el.append(thumb, info);
  return el;
}

function render() {
  const visible = state.files.filter(matches);
  $("#loading").hidden = true;
  $("#empty").hidden = visible.length > 0;
  $("#empty").textContent = state.files.length ? "Nothing matches this filter." : "No files yet. Drop the first one above.";
  $("#grid").replaceChildren(...visible.map(card));
  const total = state.files.reduce((s, f) => s + f.size, 0);
  $("#summary").textContent = state.files.length ? `${state.files.length} file${state.files.length === 1 ? "" : "s"} · ${human(total)}` : "";
}

let refreshing = null;
let again = false;
async function refresh() {
  // Coalesce bursts of change signals into one list call at a time.
  if (refreshing) {
    again = true;
    return refreshing;
  }
  refreshing = (async () => {
    try {
      state.files = await backyard.files.list({ limit: PAGE });
      render();
    } catch (err) {
      toast(`Could not load files: ${err.message}`);
    } finally {
      refreshing = null;
      if (again) {
        again = false;
        refresh();
      }
    }
  })();
  return refreshing;
}

// Tell every open tab (including this one) that the file list changed.
function signal(action, f) {
  return signals.set("files", { action, file_id: f.id, name: f.name, by: state.me?.name ?? "" }).catch(() => refresh());
}

// ---------------------------------------------------------------------------
// Upload & delete

async function upload(fileList) {
  const files = [...fileList].filter((f) => f.size > 0 || f.name);
  for (const file of files) {
    const li = document.createElement("li");
    li.innerHTML = `<span class="spinner" aria-hidden="true"></span><span class="qname"></span><span class="qstate">uploading…</span>`;
    li.querySelector(".qname").textContent = `${file.name || "pasted file"} (${human(file.size)})`;
    $("#queue").append(li);
    try {
      const name = file.name || `pasted-${Date.now()}.${(file.type.split("/")[1] || "bin").replace(/[^a-z0-9]/gi, "")}`;
      const info = await backyard.files.upload(file, { name });
      li.classList.add("done");
      li.querySelector(".qstate").textContent = "done";
      await signal("upload", info);
      setTimeout(() => li.remove(), 1500);
    } catch (err) {
      li.classList.add("failed");
      li.querySelector(".qstate").textContent =
        err.code === "too_large" ? "too large" : err.code === "rate_limited" ? "too many uploads, try again soon" : err.message;
      setTimeout(() => li.remove(), 8000);
    }
  }
}

async function remove(f) {
  if (!confirm(`Delete “${f.name}” for everyone?`)) return;
  try {
    await backyard.files.delete(f.id);
    state.files = state.files.filter((x) => x.id !== f.id);
    render();
    await signal("delete", f);
  } catch (err) {
    toast(`Could not delete: ${err.message}`);
  }
}

function openViewer(f) {
  $("#viewer-img").src = f.url;
  $("#viewer-img").alt = f.name;
  $("#viewer-caption").textContent = f.name;
  $("#viewer").showModal();
}

// ---------------------------------------------------------------------------
// Wiring

const zone = $("#zone");
$("#picker").addEventListener("change", (e) => {
  upload(e.target.files);
  e.target.value = "";
});
zone.addEventListener("keydown", (e) => {
  if (e.key === "Enter" || e.key === " ") {
    e.preventDefault();
    $("#picker").click();
  }
});
for (const type of ["dragenter", "dragover"]) {
  addEventListener(type, (e) => {
    if (![...(e.dataTransfer?.types ?? [])].includes("Files")) return;
    e.preventDefault();
    zone.classList.add("over");
  });
}
addEventListener("dragleave", (e) => {
  if (!e.relatedTarget) zone.classList.remove("over");
});
addEventListener("drop", (e) => {
  if (!e.dataTransfer?.files.length) return;
  e.preventDefault();
  zone.classList.remove("over");
  upload(e.dataTransfer.files);
});
addEventListener("paste", (e) => {
  if (e.clipboardData?.files.length) upload(e.clipboardData.files);
});
$("#viewer").addEventListener("click", (e) => {
  if (e.target === e.currentTarget) e.currentTarget.close();
});

document.querySelectorAll("[data-kind]").forEach((chip) =>
  chip.addEventListener("click", () => {
    state.kind = chip.dataset.kind;
    document.querySelectorAll("[data-kind]").forEach((c) => c.classList.toggle("active", c === chip));
    render();
  }),
);
$("#search").addEventListener("input", (e) => {
  state.query = e.target.value.trim().toLowerCase();
  render();
});

async function main() {
  state.me = await backyard.me();
  signals.subscribe({ onChange: () => refresh() });
  // After a reconnect, changes made while offline were missed: reload the list.
  backyard.socket.on("open", () => refresh());
  await refresh();
}

main().catch((err) => {
  $("#loading").textContent = `Could not load: ${err.message}`;
});
