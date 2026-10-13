/**
 * 26-character, lexicographically time-ordered ULIDs (SPEC §5.2), monotonic
 * within a millisecond like docstore.NewID.
 */

const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

let lastMs = -1;
const lastBytes = new Uint8Array(16);

/** Returns a new ULID. */
export function newULID(nowMs = Date.now()): string {
  const b = new Uint8Array(16);
  if (nowMs <= lastMs) {
    // Same (or earlier, e.g. frozen Worker clock) millisecond: increment the
    // 80-bit random part of the previous id.
    b.set(lastBytes);
    for (let i = 15; i >= 6; i--) {
      b[i] = (b[i]! + 1) & 0xff;
      if (b[i] !== 0) break;
    }
  } else {
    let ms = nowMs;
    for (let i = 5; i >= 0; i--) {
      b[i] = ms % 256;
      ms = Math.floor(ms / 256);
    }
    crypto.getRandomValues(b.subarray(6));
    lastMs = nowMs;
  }
  lastBytes.set(b);
  return encode(b);
}

function encode(b: Uint8Array): string {
  // 128 bits → 26 base32 characters (the first encodes 3 bits).
  let hi = 0n;
  let lo = 0n;
  for (let i = 0; i < 8; i++) hi = (hi << 8n) | BigInt(b[i]!);
  for (let i = 8; i < 16; i++) lo = (lo << 8n) | BigInt(b[i]!);
  const out: string[] = new Array<string>(26);
  const mask64 = (1n << 64n) - 1n;
  for (let i = 25; i >= 0; i--) {
    out[i] = CROCKFORD[Number(lo & 31n)]!;
    lo = ((lo >> 5n) | (hi << 59n)) & mask64;
    hi >>= 5n;
  }
  return out.join("");
}
