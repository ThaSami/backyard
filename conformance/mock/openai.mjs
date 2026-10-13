#!/usr/bin/env node
// A deterministic OpenAI-compatible API for the conformance runners, so both
// editions' AI endpoints (SPEC §5.4) run end to end without a real provider:
//
//   POST /v1/chat/completions   (stream and non-stream; reply "mock: <last user text>")
//   POST /v1/embeddings         (3-dimensional vectors derived from each input)
//   POST /v1/images/generations (n 1×1 PNGs as b64_json)
//
// A last user message containing "MOCK_FAIL" makes the call fail with HTTP 401
// (a provider failure the server reports as 502 upstream_error); one with
// "MOCK_LENGTH" finishes with finish_reason "length" (stop_reason max_tokens);
// one with "MOCK_IMAGES" is answered with what arrived of its images:
// "mock: images <media type>:<sha256 of the bytes>" per inline (data:) image,
// "url:<url>" per image the provider would have to download, "none" without.
//
//   node mock/openai.mjs [port]      (default 18490; prints "listening <port>")
import { createHash } from "node:crypto";
import http from "node:http";

const PNG = Buffer.from(
  "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63f8ffff3f0005fe02fea7d2a5b20000000049454e44ae426082",
  "hex",
);

function lastUserText(messages) {
  for (let i = (messages?.length ?? 0) - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.role !== "user") continue;
    if (typeof m.content === "string") return m.content;
    if (Array.isArray(m.content)) return m.content.filter((p) => p.type === "text").map((p) => p.text).join("");
  }
  return "";
}

function lastUserMessage(messages) {
  for (let i = (messages?.length ?? 0) - 1; i >= 0; i--) {
    if (messages[i].role === "user") return messages[i];
  }
  return null;
}

/** Describes the image parts of a message (see MOCK_IMAGES above). */
function describeImages(message) {
  const parts = Array.isArray(message?.content) ? message.content : [];
  const out = [];
  for (const p of parts) {
    if (p?.type !== "image_url") continue;
    const url = String(p.image_url?.url ?? "");
    const m = /^data:([^;,]*);base64,(.*)$/s.exec(url);
    if (m) out.push(`${m[1]}:${createHash("sha256").update(Buffer.from(m[2], "base64")).digest("hex")}`);
    else out.push(`url:${url}`);
  }
  return out.length ? out.join(" ") : "none";
}

function send(res, status, body) {
  const b = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(b) });
  res.end(b);
}

function chat(req, res, body) {
  const text = lastUserText(body.messages);
  if (text.includes("MOCK_FAIL")) return send(res, 401, { error: { message: "mock: invalid api key", type: "invalid_request_error" } });
  const wantsJSON = body.response_format?.type === "json_object";
  let content = wantsJSON ? JSON.stringify({ echo: text }) : `mock: ${text}`;
  if (text.includes("MOCK_IMAGES")) content = `mock: images ${describeImages(lastUserMessage(body.messages))}`;
  const usage = { prompt_tokens: 7, completion_tokens: 5, total_tokens: 12 };
  const limit = body.max_completion_tokens ?? body.max_tokens;
  const truncated = text.includes("MOCK_LENGTH");
  const finish = truncated ? "length" : "stop";
  if (!body.stream) {
    return send(res, 200, {
      id: "chatcmpl-mock",
      object: "chat.completion",
      model: body.model,
      choices: [{ index: 0, message: { role: "assistant", content }, finish_reason: finish }],
      usage,
      mock_max_tokens: limit,
    });
  }
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
  const chunk = (delta, finishReason = null, extra = {}) =>
    res.write(`data: ${JSON.stringify({ id: "chatcmpl-mock", object: "chat.completion.chunk", model: body.model, choices: [{ index: 0, delta, finish_reason: finishReason }], ...extra })}\n\n`);
  chunk({ role: "assistant", content: "" });
  const parts = [content.slice(0, 3), content.slice(3, 7), content.slice(7)].filter(Boolean);
  for (const p of parts) chunk({ content: p });
  chunk({}, finish);
  if (body.stream_options?.include_usage) {
    res.write(`data: ${JSON.stringify({ id: "chatcmpl-mock", object: "chat.completion.chunk", model: body.model, choices: [], usage })}\n\n`);
  }
  res.end("data: [DONE]\n\n");
}

function embed(res, body) {
  const inputs = Array.isArray(body.input) ? body.input : [body.input];
  send(res, 200, {
    object: "list",
    model: body.model,
    data: inputs.map((s, index) => ({ object: "embedding", index, embedding: [String(s).length, 1, 0.5] })),
    usage: { prompt_tokens: inputs.length * 2, total_tokens: inputs.length * 2 },
  });
}

function image(res, body) {
  const n = Math.max(1, Number(body.n) || 1);
  send(res, 200, { created: 0, data: Array.from({ length: n }, () => ({ b64_json: PNG.toString("base64") })) });
}

const server = http.createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    let body = {};
    try {
      body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString("utf8")) : {};
    } catch {
      return send(res, 400, { error: { message: "invalid JSON" } });
    }
    const path = new URL(req.url, "http://mock").pathname.replace(/\/+$/, "");
    if (req.method === "POST" && path.endsWith("/chat/completions")) return chat(req, res, body);
    if (req.method === "POST" && path.endsWith("/embeddings")) return embed(res, body);
    if (req.method === "POST" && path.endsWith("/images/generations")) return image(res, body);
    if (req.method === "GET" && path.endsWith("/models")) return send(res, 200, { object: "list", data: [] });
    send(res, 404, { error: { message: `mock: no route for ${req.method} ${path}` } });
  });
});

const port = Number(process.argv[2] || process.env.PORT || 18490);
server.listen(port, "127.0.0.1", () => {
  console.log(`listening ${port}`);
});
for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => server.close(() => process.exit(0)));
