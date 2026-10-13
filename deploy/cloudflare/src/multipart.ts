/**
 * Streaming multipart/form-data reader (RFC 7578, RFC 2046 §5.1) for uploads
 * too large to buffer in a Worker's 128 MB of memory: parts are read one at a
 * time and the body of a part is handed out in chunks as it arrives, so memory
 * stays bounded whatever the size of the upload.
 */

/** A malformed multipart body (the caller answers 400). */
export class MultipartError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "MultipartError";
  }
}

/** Headers of one part that matter for form data. */
export interface PartInfo {
  /** Form field name (Content-Disposition name). */
  name: string;
  /** File name, or null for a plain (non-file) field. */
  filename: string | null;
  /** Content-Type of the part ("" when absent). */
  contentType: string;
}

const enc = new TextEncoder();
const dec = new TextDecoder();
const CRLF = enc.encode("\r\n");
/** Bound of one header line and of all headers of a part. */
const MAX_HEADER_LINE = 8 << 10;
const MAX_HEADERS = 32;

/** Extracts the boundary of a multipart/form-data Content-Type. */
export function boundaryOf(contentType: string): string {
  const [type, ...params] = splitParams(contentType);
  if ((type ?? "").toLowerCase() !== "multipart/form-data") throw new MultipartError("expected a multipart/form-data body");
  for (const p of params) {
    if (p.key === "boundary") {
      // RFC 2046: 1-70 characters, no trailing space.
      if (!/^[0-9A-Za-z'()+_,\-./:=? ]{0,69}[0-9A-Za-z'()+_,\-./:=?]$/.test(p.value)) throw new MultipartError("invalid multipart boundary");
      return p.value;
    }
  }
  throw new MultipartError("the multipart Content-Type has no boundary");
}

/**
 * Splits "type; k=v; k2="quoted; value"" into the type and its parameters
 * (keys lowercased, quoted strings unescaped).
 */
export function splitParams(header: string): [string, ...{ key: string; value: string }[]] {
  const out: { key: string; value: string }[] = [];
  let i = header.indexOf(";");
  const type = (i < 0 ? header : header.slice(0, i)).trim();
  while (i >= 0 && i < header.length) {
    i++; // past ";"
    while (i < header.length && (header[i] === " " || header[i] === "\t")) i++;
    const eq = header.indexOf("=", i);
    const semi = header.indexOf(";", i);
    if (eq < 0 || (semi >= 0 && semi < eq)) {
      // A parameter without a value: skip it.
      i = semi;
      continue;
    }
    const key = header.slice(i, eq).trim().toLowerCase();
    let j = eq + 1;
    while (j < header.length && (header[j] === " " || header[j] === "\t")) j++;
    let value = "";
    if (header[j] === '"') {
      j++;
      while (j < header.length && header[j] !== '"') {
        // Browsers send Windows paths unescaped: only \" and \\ are escapes.
        if (header[j] === "\\" && (header[j + 1] === '"' || header[j + 1] === "\\")) j++;
        value += header[j];
        j++;
      }
      j++; // closing quote
      const next = header.indexOf(";", j);
      i = next;
    } else {
      const next = header.indexOf(";", j);
      value = (next < 0 ? header.slice(j) : header.slice(j, next)).trim();
      i = next;
    }
    if (key) out.push({ key, value });
  }
  return [type, ...out];
}

/** Parses a part's Content-Disposition (form-data; name="…"; filename="…"). */
export function parseDisposition(v: string): { name: string; filename: string | null } {
  const [type, ...params] = splitParams(v);
  if ((type ?? "").toLowerCase() !== "form-data") throw new MultipartError("a part is not form-data");
  let name: string | null = null;
  let filename: string | null = null;
  let extended: string | null = null;
  for (const p of params) {
    if (p.key === "name") name = p.value;
    else if (p.key === "filename") filename = p.value;
    else if (p.key === "filename*") {
      // RFC 5987: charset'lang'percent-encoded
      const m = /^([^']*)'[^']*'(.*)$/.exec(p.value);
      if (m && (m[1] ?? "").toLowerCase() === "utf-8") {
        try {
          extended = decodeURIComponent(m[2] ?? "");
        } catch {
          /* keep the plain filename */
        }
      }
    }
  }
  if (name === null) throw new MultipartError("a part has no field name");
  return { name, filename: extended ?? filename };
}

/** Index of needle in haystack at or after from, or -1. */
export function indexOfBytes(haystack: Uint8Array, needle: Uint8Array, from = 0): number {
  const first = needle[0]!;
  const last = haystack.length - needle.length;
  for (let i = haystack.indexOf(first, from); i >= 0 && i <= last; i = haystack.indexOf(first, i + 1)) {
    let j = 1;
    while (j < needle.length && haystack[i + j] === needle[j]) j++;
    if (j === needle.length) return i;
  }
  return -1;
}

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
  if (a.length === 0) return b;
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

/**
 * Reads a multipart body part by part:
 *
 *   for (let p = await r.next(); p; p = await r.next()) {
 *     if (p.name === "file") await r.body((chunk) => …); // or r.text(max)
 *   }
 *
 * A part whose body is not read is skipped by the next call to next().
 */
export class MultipartReader {
  private readonly reader: ReadableStreamDefaultReader<Uint8Array>;
  /** "\r\n--" + boundary: what ends a part's body. */
  private readonly delimiter: Uint8Array;
  private buf: Uint8Array;
  private state: "preamble" | "delimiter" | "body" | "done" = "preamble";

  constructor(body: ReadableStream<Uint8Array>, boundary: string) {
    this.reader = body.getReader();
    this.delimiter = enc.encode("\r\n--" + boundary);
    // The first boundary has no leading CRLF: pretend it has one.
    this.buf = CRLF;
  }

  /** Reads one more chunk into the buffer; false at the end of the stream. */
  private async fill(): Promise<boolean> {
    for (;;) {
      const { done, value } = await this.reader.read();
      if (done) return false;
      if (value && value.length) {
        this.buf = concat(this.buf, value);
        return true;
      }
    }
  }

  /** Makes at least n bytes available (false when the stream ends first). */
  private async ensure(n: number): Promise<boolean> {
    while (this.buf.length < n) if (!(await this.fill())) return false;
    return true;
  }

  /** Reads one CRLF-terminated line (without the CRLF). */
  private async line(): Promise<string> {
    for (;;) {
      const i = indexOfBytes(this.buf, CRLF);
      if (i >= 0) {
        if (i > MAX_HEADER_LINE) throw new MultipartError("multipart header line too long");
        const s = dec.decode(this.buf.subarray(0, i));
        this.buf = this.buf.subarray(i + 2);
        return s;
      }
      if (this.buf.length > MAX_HEADER_LINE) throw new MultipartError("multipart header line too long");
      if (!(await this.fill())) throw new MultipartError("unexpected end of the multipart body");
    }
  }

  /**
   * Hands the body of the current part to onChunk (chunks are views into an
   * internal buffer: copy what you keep) and returns its size.
   */
  async body(onChunk: (chunk: Uint8Array) => void | Promise<void>): Promise<number> {
    if (this.state !== "body") throw new Error("multipart: no part body to read");
    const keep = this.delimiter.length - 1;
    let size = 0;
    for (;;) {
      const i = indexOfBytes(this.buf, this.delimiter);
      if (i >= 0) {
        if (i > 0) await onChunk(this.buf.subarray(0, i));
        size += i;
        this.buf = this.buf.subarray(i + this.delimiter.length);
        this.state = "delimiter";
        return size;
      }
      if (this.buf.length > keep) {
        const n = this.buf.length - keep;
        await onChunk(this.buf.subarray(0, n));
        size += n;
        this.buf = this.buf.slice(n);
      }
      if (!(await this.fill())) throw new MultipartError("unexpected end of the multipart body");
    }
  }

  /** Reads the current part's body as UTF-8 text of at most max bytes. */
  async text(max: number): Promise<string> {
    const parts: Uint8Array[] = [];
    let total = 0;
    await this.body((c) => {
      total += c.length;
      if (total > max) throw new MultipartError(`a form field is larger than ${max} bytes`);
      parts.push(c.slice());
    });
    let all: Uint8Array = new Uint8Array(0);
    for (const p of parts) all = concat(all, p);
    return dec.decode(all);
  }

  /** Advances to the next part (skipping the rest of the current one); null at the end. */
  async next(): Promise<PartInfo | null> {
    if (this.state === "done") return null;
    if (this.state === "body") await this.body(() => {});
    if (this.state === "preamble") {
      // Skip everything up to the first delimiter.
      this.state = "body";
      await this.body(() => {});
    }
    // Just after a delimiter: "--" ends the body, else (padding and) CRLF.
    if (!(await this.ensure(2))) throw new MultipartError("unexpected end of the multipart body");
    if (this.buf[0] === 0x2d && this.buf[1] === 0x2d) {
      this.state = "done";
      await this.reader.cancel().catch(() => {});
      return null;
    }
    const pad = await this.line();
    if (pad.trim() !== "") throw new MultipartError("malformed multipart boundary line");
    let disposition = "";
    let contentType = "";
    for (let n = 0; ; n++) {
      if (n > MAX_HEADERS) throw new MultipartError("too many multipart headers");
      const h = await this.line();
      if (h === "") break;
      const colon = h.indexOf(":");
      if (colon <= 0) throw new MultipartError("malformed multipart header");
      const key = h.slice(0, colon).trim().toLowerCase();
      const value = h.slice(colon + 1).trim();
      if (key === "content-disposition") disposition = value;
      else if (key === "content-type") contentType = value;
    }
    if (!disposition) throw new MultipartError("a part has no Content-Disposition");
    const { name, filename } = parseDisposition(disposition);
    this.state = "body";
    return { name, filename, contentType };
  }

  /** Releases the underlying stream. */
  async cancel(): Promise<void> {
    this.state = "done";
    await this.reader.cancel().catch(() => {});
  }
}
