// A fake OpenAI-compatible upstream for the Go server's "openai" provider, so
// every AI flow (chat, SSE streaming, image generation, embeddings) runs
// offline and deterministically.
//
// Prompt markers change its behavior:
//   [slow]  stream the answer slowly (150 ms per chunk instead of 30 ms)
//   [fail]  answer 500 (the Backyard server turns that into 502 upstream_error)
//   [cut]   stream a few chunks, then drop the connection mid-answer
//
// GET /__requests returns the recorded requests (newest last) so tests can
// check what the Backyard server sent upstream.
import http from "node:http";

/** A 1×1 PNG (returned by the fake image model). */
export const PNG_1X1 = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
  "base64",
);

/** The API key the e2e servers are configured with; anything else is rejected. */
export const FAKE_AI_KEY = "e2e-fake-key";

/** The fake chat model's (Markdown) answer to the user's last message. */
export function fakeReply(text) {
  return `**Hello** from the fake model! You said: ${text}\n\n- first point\n- second point with \`code\``;
}

/** The embedding vector the fake model returns for one input string. */
export function fakeEmbedding(text) {
  return [String(text).length, 0.5, -1];
}

/** Splits text into the chunks the fake model streams (words with their trailing space). */
export function fakeChunks(text) {
  return text.match(/\S+\s*|\s+/g) ?? [];
}

function lastUserText(messages) {
  const last = (messages ?? []).filter((m) => m.role === "user").pop();
  if (!last) return "";
  if (typeof last.content === "string") return last.content;
  return (last.content ?? []).map((p) => p.text ?? "").join("");
}

/**
 * Starts the fake upstream on 127.0.0.1:port (0 = any free port).
 * @returns {Promise<{url: string, origin: string, close: () => Promise<void>}>}
 */
export async function startFakeAI({ port = 0 } = {}) {
  const requests = [];
  const sockets = new Set();

  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const path = new URL(req.url, "http://fake").pathname;
      const send = (status, obj) => {
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(obj));
      };
      if (path === "/__requests") {
        if (req.method === "DELETE") requests.length = 0;
        return send(200, requests);
      }
      let body = {};
      try {
        body = JSON.parse(Buffer.concat(chunks).toString() || "{}");
      } catch {
        return send(400, { error: { message: "invalid JSON", type: "invalid_request_error" } });
      }
      requests.push({ method: req.method, path, body, authorization: req.headers.authorization ?? null });
      if (requests.length > 500) requests.shift();
      if (req.headers.authorization !== `Bearer ${FAKE_AI_KEY}`) {
        return send(401, { error: { message: "invalid api key", type: "invalid_request_error" } });
      }

      if (req.method === "POST" && path === "/v1/chat/completions") {
        const text = lastUserText(body.messages);
        if (text.includes("[fail]")) return send(500, { error: { message: "the fake model failed on purpose", type: "server_error" } });
        const reply = body.response_format ? '{"ok": true}' : fakeReply(text);
        const pieces = fakeChunks(reply);
        const usage = { prompt_tokens: 7, completion_tokens: pieces.length, total_tokens: 7 + pieces.length };
        if (!body.stream) {
          return send(200, {
            id: "chatcmpl-e2e",
            object: "chat.completion",
            model: body.model,
            choices: [{ index: 0, message: { role: "assistant", content: reply }, finish_reason: "stop" }],
            usage,
          });
        }
        res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
        const event = (obj) => res.write(`data: ${JSON.stringify(obj)}\n\n`);
        const chunk = (delta, finish = null) => ({
          id: "chatcmpl-e2e",
          object: "chat.completion.chunk",
          model: body.model,
          choices: [{ index: 0, delta, finish_reason: finish }],
        });
        const delay = text.includes("[slow]") ? 150 : 30;
        const cutAt = text.includes("[cut]") ? 3 : Infinity;
        let i = 0;
        const timer = setInterval(() => {
          if (i === 0) event(chunk({ role: "assistant", content: "" }));
          if (i >= cutAt) {
            clearInterval(timer);
            res.destroy();
            return;
          }
          if (i < pieces.length) {
            event(chunk({ content: pieces[i++] }));
            return;
          }
          clearInterval(timer);
          event(chunk({}, "stop"));
          event({ id: "chatcmpl-e2e", object: "chat.completion.chunk", model: body.model, choices: [], usage });
          res.end("data: [DONE]\n\n");
        }, delay);
        res.on("close", () => clearInterval(timer));
        return;
      }

      if (req.method === "POST" && path === "/v1/images/generations") {
        const n = Math.max(1, Math.min(4, Number(body.n) || 1));
        return send(200, { created: Math.floor(Date.now() / 1000), data: Array.from({ length: n }, () => ({ b64_json: PNG_1X1.toString("base64") })) });
      }

      if (req.method === "POST" && path === "/v1/embeddings") {
        const input = Array.isArray(body.input) ? body.input : [body.input];
        return send(200, {
          object: "list",
          model: body.model,
          data: input.map((t, index) => ({ object: "embedding", index, embedding: fakeEmbedding(t) })),
          usage: { prompt_tokens: input.length, total_tokens: input.length },
        });
      }

      send(404, { error: { message: `no such endpoint: ${req.method} ${path}`, type: "invalid_request_error" } });
    });
  });
  server.on("connection", (s) => {
    sockets.add(s);
    s.on("close", () => sockets.delete(s));
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, "127.0.0.1", resolve);
  });
  const origin = `http://127.0.0.1:${server.address().port}`;
  return {
    url: `${origin}/v1`,
    origin,
    close: () =>
      new Promise((resolve) => {
        for (const s of sockets) s.destroy();
        server.close(() => resolve());
      }),
  };
}
