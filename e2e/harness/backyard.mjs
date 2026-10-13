// Builds the backyard binary, runs `backyard serve` instances with throwaway
// configurations, and runs CLI commands (deploy, init) against them.
import { spawn, spawnSync } from "node:child_process";
import { accessSync, constants, closeSync, mkdirSync, openSync, readFileSync, writeFileSync } from "node:fs";
import net from "node:net";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

/** The repository root. */
export const REPO = fileURLToPath(new URL("../../", import.meta.url));

/** Environment variables the server and CLI read; the caller's values must not leak in. */
const BACKYARD_ENV = /^(BACKYARD_|DATABASE_URL$|PORT$)/;

/** process.env without Backyard's own variables (plus extra). */
export function cleanEnv(extra = {}) {
  const env = {};
  for (const [k, v] of Object.entries(process.env)) if (!BACKYARD_ENV.test(k)) env[k] = v;
  return { ...env, ...extra };
}

/**
 * Returns the backyard binary: $BACKYARD_BIN when set (e.g. a CI build), else
 * `go build ./cmd/backyard` into dir.
 */
export function buildBackyard(dir) {
  const prebuilt = process.env.BACKYARD_BIN;
  if (prebuilt) {
    const bin = resolve(prebuilt);
    accessSync(bin, constants.X_OK);
    return bin;
  }
  const bin = join(dir, "backyard");
  const built = spawnSync("go", ["build", "-o", bin, "./cmd/backyard"], {
    cwd: REPO,
    stdio: ["ignore", "inherit", "inherit"],
    env: { ...process.env, CGO_ENABLED: "0" },
    timeout: 600_000,
  });
  if (built.status !== 0) throw new Error(`go build ./cmd/backyard failed (status ${built.status}, ${built.error ?? "no error"})`);
  return bin;
}

const running = new Set();
// Never leave servers behind, even when the runner dies abruptly.
process.once("exit", () => {
  for (const p of running) {
    try {
      p.kill("SIGKILL");
    } catch {
      /* already gone */
    }
  }
});

/** One `backyard serve` process with its own directory, config and log file. */
export class BackyardServer {
  /**
   * @param {{bin: string, name: string, port: number, dir: string, config: object, dev?: boolean, url?: string}} opts
   *   config is written as JSON (valid YAML); url is the hub URL (default http://localhost:<port>).
   */
  constructor({ bin, name, port, dir, config, dev = true, url }) {
    Object.assign(this, { bin, name, port, dir, config, dev });
    this.url = url ?? `http://localhost:${port}`;
    this.logFile = join(dir, "server.log");
    this.configFile = join(dir, "backyard.yaml");
    this.proc = null;
    mkdirSync(dir, { recursive: true });
    writeFileSync(this.configFile, JSON.stringify(config, null, 2));
  }

  /** Starts the server and waits until /__backyard/healthz answers. */
  async start({ timeoutMs = 60_000 } = {}) {
    if (this.proc) throw new Error(`${this.name}: already running`);
    // A leftover server on the port would answer the health check for us.
    if (await portInUse(this.port)) {
      throw new Error(`${this.name}: port ${this.port} is already in use (a server left over from an earlier run?); set BACKYARD_E2E_PORT to move all ports`);
    }
    const out = openSync(this.logFile, "a");
    const args = ["serve", "--config", this.configFile];
    if (this.dev) args.push("--dev");
    const proc = spawn(this.bin, args, { cwd: this.dir, env: cleanEnv(), stdio: ["ignore", out, out] });
    closeSync(out);
    this.proc = proc;
    running.add(proc);
    proc.once("exit", () => running.delete(proc));
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      if (proc.exitCode !== null || proc.signalCode !== null) {
        this.proc = null;
        throw new Error(`${this.name}: backyard serve exited (${proc.exitCode ?? proc.signalCode}):\n${this.logTail()}`);
      }
      try {
        const res = await fetch(`http://127.0.0.1:${this.port}/__backyard/healthz`, { signal: AbortSignal.timeout(2000) });
        if (res.ok) return this;
      } catch {
        /* not listening yet */
      }
      if (Date.now() > deadline) {
        await this.kill();
        throw new Error(`${this.name}: not healthy after ${timeoutMs} ms:\n${this.logTail()}`);
      }
      await new Promise((r) => setTimeout(r, 150));
    }
  }

  /** Stops gracefully (SIGTERM, then SIGKILL after 10 s). */
  async stop() {
    await this.#signal("SIGTERM", 10_000);
  }

  /** Kills the process at once (a crash, as far as clients can tell). */
  async kill() {
    await this.#signal("SIGKILL", 5_000);
  }

  async #signal(sig, graceMs) {
    const proc = this.proc;
    if (!proc) return;
    this.proc = null;
    if (proc.exitCode !== null || proc.signalCode !== null) return;
    const exited = new Promise((r) => proc.once("exit", r));
    proc.kill(sig);
    const timer = setTimeout(() => proc.kill("SIGKILL"), graceMs);
    await exited;
    clearTimeout(timer);
  }

  /** The last lines of the server log. */
  logTail(lines = 80) {
    try {
      return readFileSync(this.logFile, "utf8").split("\n").slice(-lines).join("\n");
    } catch {
      return "(no log)";
    }
  }
}

/** Whether something accepts connections on 127.0.0.1:port. */
function portInUse(port) {
  return new Promise((resolve) => {
    const socket = net.connect({ host: "127.0.0.1", port });
    socket.once("connect", () => {
      socket.destroy();
      resolve(true);
    });
    socket.once("error", () => resolve(false));
    socket.setTimeout(1000, () => {
      socket.destroy();
      resolve(false);
    });
  });
}

/**
 * Runs the backyard CLI with an isolated config home and returns stdout.
 * Throws with the full output when it fails.
 */
export function backyardCLI(bin, args, { cwd = REPO, home, env = {} } = {}) {
  if (!home) throw new Error("backyardCLI: home (an isolated CLI config directory) is required");
  mkdirSync(home, { recursive: true });
  const res = spawnSync(bin, args, {
    cwd,
    env: cleanEnv({ HOME: home, XDG_CONFIG_HOME: home, NO_COLOR: "1", ...env }),
    encoding: "utf8",
    timeout: 120_000,
  });
  if (res.status !== 0) {
    throw new Error(`backyard ${args.join(" ")} failed (status ${res.status}, ${res.error ?? "no error"}):\n${res.stdout}\n${res.stderr}`);
  }
  return res.stdout;
}

/**
 * Deploys dir as site through the API method and returns the CLI's JSON
 * result ({url, ...}).
 */
export function deploySite(bin, { dir, site, server, token, home }) {
  const args = ["deploy", dir, "--site", site, "--server", server, "--method", "api", "--yes", "--json"];
  if (token) args.push("--token", token);
  const out = backyardCLI(bin, args, { home });
  try {
    return JSON.parse(out);
  } catch {
    throw new Error(`backyard deploy --json printed no JSON:\n${out}`);
  }
}
