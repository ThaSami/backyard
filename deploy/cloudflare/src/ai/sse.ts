/** Incremental text/event-stream parsing for upstream AI streams. */

/** One server-sent event. */
export interface SSEEvent {
  event: string;
  data: string;
}

/**
 * Reads a text/event-stream body and calls onEvent for every complete event
 * (multi-line data fields are joined with "\n"). Resolves at end of stream.
 */
export async function readSSE(body: ReadableStream<Uint8Array>, onEvent: (e: SSEEvent) => Promise<void> | void): Promise<void> {
  const reader = body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  let event = "";
  let data: string[] = [];
  const dispatch = async () => {
    if (data.length) await onEvent({ event: event || "message", data: data.join("\n") });
    event = "";
    data = [];
  };
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += value;
    let nl: number;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl).replace(/\r$/, "");
      buf = buf.slice(nl + 1);
      if (line === "") {
        await dispatch();
        continue;
      }
      if (line.startsWith(":")) continue;
      const colon = line.indexOf(":");
      const field = colon < 0 ? line : line.slice(0, colon);
      let val = colon < 0 ? "" : line.slice(colon + 1);
      if (val.startsWith(" ")) val = val.slice(1);
      if (field === "data") data.push(val);
      else if (field === "event") event = val;
    }
  }
  if (buf.trim() !== "") {
    const line = buf;
    if (line.startsWith("data:")) data.push(line.slice(5).replace(/^ /, ""));
  }
  await dispatch();
}
