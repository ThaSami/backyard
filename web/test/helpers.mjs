// Loads the hub scripts into a fresh vm context (no DOM: only the pure helpers).
import vm from "node:vm";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

export const HUB_DIR = fileURLToPath(new URL("../hub/", import.meta.url));

/** Loads web/hub/<file> and returns the sandbox global. */
export function loadHubScript(file, extra = {}) {
  const sandbox = {
    URL,
    TextEncoder,
    TextDecoder,
    Blob,
    File,
    setTimeout,
    clearTimeout,
    console,
    ...extra,
  };
  vm.createContext(sandbox);
  vm.runInContext(readFileSync(HUB_DIR + file, "utf8"), sandbox, { filename: file });
  return sandbox;
}

/** Converts sandbox-realm data to plain outer-realm data. */
export function plain(v) {
  return v === undefined ? undefined : JSON.parse(JSON.stringify(v));
}
