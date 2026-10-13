// examples/ai-chat against the fake OpenAI-compatible upstream: model picker,
// token-by-token streaming, Markdown rendering, history, errors; plus the
// other AI calls of the SDK (image, embeddings, JSON mode).
import { fakeChunks, fakeEmbedding, fakeReply } from "../../harness/fake-ai.mjs";
import { expect, openPage, rand, siteURL, test } from "../fixtures.mjs";

const STYLES = {
  Helpful: "You are a helpful assistant for employees of our company. Be accurate and friendly.",
  Concise: "You are a helpful assistant. Answer as briefly as possible; use bullet points when listing.",
};

/** The chat requests the fake upstream received whose last user message contains marker. */
async function upstreamChats(state, marker) {
  const all = await (await fetch(`${state.fakeAI.origin}/__requests`)).json();
  return all.filter(
    (r) => r.path === "/v1/chat/completions" && JSON.stringify(r.body.messages.filter((m) => m.role === "user").at(-1) ?? {}).includes(marker),
  );
}

/** Records every distinct text of the streaming answer bubble (to prove tokens render as they arrive). */
async function recordStreaming(page) {
  await page.evaluate(() => {
    window.__seen = [];
    new MutationObserver(() => {
      const body = document.querySelector(".msg.assistant.streaming .body");
      const text = body?.textContent ?? "";
      if (text && window.__seen.at(-1) !== text) window.__seen.push(text);
    }).observe(document.querySelector("#log"), { childList: true, subtree: true, characterData: true });
  });
}

test("streams answers token by token and keeps the conversation", async ({ newUser, state, pageProblems }) => {
  // The "[fail]" message below, should the server answer a failed stream start with an HTTP 502.
  pageProblems.allow(/the server responded with a status of 502/);
  const page = await openPage(await newUser(), siteURL(state, "ai-chat"));

  // Only chat models are offered; the server default is preselected.
  const model = page.locator("#model");
  await expect(model).toBeEnabled();
  await expect(model.locator("option")).toHaveText(["quick — Smaller fake model", "smart — Fake chat model"]);
  await expect(model).toHaveValue("smart");
  await expect(page.locator("#style option")).toHaveCount(4);
  await expect(page.locator("#suggestions .chip")).toHaveCount(4);
  await expect(page.locator("#banner")).toBeHidden();

  // First message (slow stream) via Enter.
  const marker = rand();
  const first = `Plan a team lunch [slow] ${marker}`;
  await recordStreaming(page);
  await page.locator("#input").fill(first);
  await page.locator("#input").press("Enter");
  await expect(page.locator("#welcome")).toBeHidden();
  await expect(page.locator(".msg.user .body")).toHaveText([first]);
  await expect(page.locator(".msg.assistant.streaming")).toBeVisible();
  await expect(page.locator("#send")).toBeDisabled();

  const reply = fakeReply(first);
  const answer = page.locator(".msg.assistant:not(.streaming)");
  await expect(answer).toHaveCount(1, { timeout: 20_000 });
  await expect(page.locator("#send")).toBeEnabled();
  await expect(page.locator("#input")).toBeFocused();

  // Several growing partial renders were on screen before the answer completed.
  const seen = await page.evaluate(() => window.__seen);
  expect(seen.length, `partial renders: ${JSON.stringify(seen)}`).toBeGreaterThanOrEqual(3);
  const finalText = await answer.locator(".body").textContent();
  for (let i = 1; i < seen.length; i++) expect(seen[i].length).toBeGreaterThan(seen[i - 1].length);
  expect(seen[0].length).toBeLessThan(finalText.length);

  // The Markdown answer renders safely: bold, inline code, a list.
  await expect(answer.locator(".body strong")).toHaveText("Hello");
  await expect(answer.locator(".body p").first()).toHaveText(`Hello from the fake model! You said: ${first}`);
  await expect(answer.locator(".body li")).toHaveText(["first point", "second point with code"]);
  await expect(answer.locator(".body li code")).toHaveText("code");
  await expect(answer.locator(".meta")).toHaveText(`smart · ${fakeChunks(reply).length} tokens`);

  // What the server sent upstream: the alias' model, the style as system prompt, streaming.
  const [sent] = await upstreamChats(state, marker);
  expect(sent.body.model).toBe("fake-chat");
  expect(sent.body.stream).toBe(true);
  expect(sent.body.max_tokens ?? sent.body.max_completion_tokens).toBe(2048);
  expect(sent.body.messages[0]).toEqual({ role: "system", content: STYLES.Helpful });
  expect(sent.body.messages.slice(1)).toEqual([{ role: "user", content: first }]);

  // Second message with another model and style: the history goes along.
  await model.selectOption("quick");
  await page.locator("#style").selectOption("Concise");
  const second = `And dessert? ${marker}-2`;
  await page.locator("#input").fill(second);
  await page.locator("#send").click();
  await expect(page.locator(".msg.assistant:not(.streaming)")).toHaveCount(2);
  await expect(page.locator(".msg.assistant .meta").last()).toHaveText(`quick · ${fakeChunks(fakeReply(second)).length} tokens`);
  const [sent2] = await upstreamChats(state, `${marker}-2`);
  expect(sent2.body.model).toBe("fake-chat-mini");
  expect(sent2.body.messages.map((m) => m.role)).toEqual(["system", "user", "assistant", "user"]);
  expect(sent2.body.messages[0].content).toBe(STYLES.Concise);
  expect(sent2.body.messages[2].content).toBe(reply);

  // A reload keeps the conversation (this tab) and the chosen model and style (this browser).
  await page.reload();
  await expect(page.locator(".msg.user")).toHaveCount(2);
  await expect(page.locator(".msg.assistant")).toHaveCount(2);
  await expect(page.locator("#model")).toHaveValue("quick");
  await expect(page.locator("#style")).toHaveValue("Concise");

  // An upstream failure is explained and the message is kept for a retry.
  const failing = `[fail] ${marker}`;
  await page.locator("#input").fill(failing);
  await page.locator("#input").press("Enter");
  await expect(page.locator("#banner")).toHaveText("The AI provider had a problem. Please try again.");
  await expect(page.locator("#input")).toHaveValue(failing);
  await expect(page.locator(".msg.user")).toHaveCount(2);
  await expect(page.locator(".msg.assistant")).toHaveCount(2);

  // So is an answer that breaks off mid-stream: the partial answer is dropped.
  const cut = `[cut] ${marker}`;
  await page.locator("#input").fill(cut);
  await page.locator("#input").press("Enter");
  await expect(page.locator("#banner")).toHaveText("The AI provider had a problem. Please try again.");
  await expect(page.locator("#input")).toHaveValue(cut);
  await expect(page.locator(".msg.assistant")).toHaveCount(2);
  await expect(page.locator(".msg.streaming")).toHaveCount(0);
  await expect(page.locator("#send")).toBeEnabled();

  // "New chat" starts over.
  await page.locator("#new-chat").click();
  await expect(page.locator("#welcome")).toBeVisible();
  await expect(page.locator(".msg")).toHaveCount(0);
  await expect(page.locator("#banner")).toBeHidden();
});

test("a suggestion chip sends its prompt", async ({ newUser, state }) => {
  const page = await openPage(await newUser(), siteURL(state, "ai-chat"));
  const chip = page.locator("#suggestions .chip", { hasText: "Explain websockets in three sentences" });
  await chip.click();
  await expect(page.locator(".msg.user .body")).toHaveText(["Explain websockets in three sentences"]);
  await expect(page.locator(".msg.assistant:not(.streaming) .body strong")).toHaveText("Hello");
  // A chip ending in "…" only fills the input.
  await page.locator("#new-chat").click();
  await page.locator("#suggestions .chip", { hasText: "Turn these notes into action items" }).click();
  await expect(page.locator("#input")).toHaveValue("Turn these notes into action items: ");
  await expect(page.locator("#input")).toBeFocused();
  await expect(page.locator(".msg")).toHaveCount(0);
});

test("the SDK's other AI calls work through the server: image, embeddings, JSON, models", async ({ newUser, state }) => {
  const page = await openPage(await newUser(), siteURL(state, "ai-chat"));
  const r = await page.evaluate(async () => {
    const img = await backyard.ai.image("a tiny dot", { n: 2 });
    const el = Object.assign(new Image(), { src: img.url });
    await el.decode();
    const file = await backyard.files.get(img.id);
    const json = await backyard.ai.chat("answer in JSON", { json: true });
    const models = await backyard.ai.models();
    return {
      image: { n: img.images.length, type: img.content_type, model: img.model, width: el.naturalWidth, fileType: file.content_type },
      one: await backyard.ai.embed("abc"),
      many: await backyard.ai.embed(["a", "bb"]),
      json: json.json,
      models: { default: models.default, kinds: models.models.map((m) => `${m.name}:${m.kind}`).sort() },
    };
  });
  expect(r.image).toEqual({ n: 2, type: "image/png", model: "image", width: 1, fileType: "image/png" });
  expect(r.one).toEqual(fakeEmbedding("abc"));
  expect(r.many).toEqual([fakeEmbedding("a"), fakeEmbedding("bb")]);
  expect(r.json).toEqual({ ok: true });
  expect(r.models).toEqual({ default: "smart", kinds: ["embed:embedding", "image:image", "quick:chat", "smart:chat"] });
});
