// Terminal output and prompts of the engine (backyard-cloudflare), shared by
// every command: colors (off with NO_COLOR or when stdout is not a terminal),
// the CI and secret-display rules, the interactive prompts, and CliError, whose
// code is the process exit code:
//
//   0    done, or nothing to do
//   1    a Cloudflare, wrangler, network or file operation failed (a re-run resumes)
//   2    usage error, or input/confirmation needed in non-interactive mode
//   3    refused for safety (conflict, foreign object, auth downgrade, unknown
//        live state, domain change while Access is on): nothing was changed
//   4    applied, but verification is pending or failed (a re-run resumes)
//   130  interrupted
//
// "Interactive" means stdin and stdout are terminals, CI is not set and
// --non-interactive was not given. Prompts never run otherwise: ask() and
// askHidden() exit 2 naming the flag to pass, confirm() is true only with --yes.

import { createInterface } from "node:readline";

/** Exit codes of every engine command (and of `backyard cloudflare`). */
export const EXIT = Object.freeze({ OK: 0, FAILED: 1, USAGE: 2, REFUSED: 3, PENDING: 4, INTERRUPTED: 130 });

/**
 * A failure meant for the person running the command; `code` is the exit code
 * (EXIT). `applied` lists what was already changed (printed after the message,
 * so a reader knows what a re-run resumes from).
 */
export class CliError extends Error {
  /**
   * @param {number} code exit code: 1, 2, 3, 4 or 130
   * @param {string} message
   * @param {{applied?: string[], cause?: unknown, reason?: string}} [details]
   */
  constructor(code, message, details = {}) {
    super(message, details.cause !== undefined ? { cause: details.cause } : undefined);
    this.name = "CliError";
    /** @type {number} */
    this.code = code;
    /** @type {string[]} */
    this.applied = details.applied ?? [];
    /** @type {string} machine-readable cause (e.g. the auth guard's "downgrade") */
    this.reason = details.reason ?? "";
  }
}

const useColor = () => Boolean(process.stdout.isTTY) && !process.env.NO_COLOR;
const paint = (code) => (s) => (useColor() ? `\x1b[${code}m${s}\x1b[0m` : String(s));
export const bold = paint("1");
export const dim = paint("2");
export const green = paint("32");
export const yellow = paint("33");
export const cyan = paint("36");
export const red = paint("31");

/** Line-oriented output: results and warnings go to stdout, errors to stderr. */
export const ui = {
  /** @param {...unknown} a */
  log: (...a) => console.log(...a),
  /** @param {string} msg */
  ok: (msg) => console.log(`${green("✓")} ${msg}`),
  /** @param {string} msg */
  warn: (msg) => console.log(`${yellow("!")} ${msg}`),
  /** @param {string} msg */
  note: (msg) => console.log(dim(msg)),
  /** @param {string} msg */
  error: (msg) => console.error(`${red("✗")} ${msg}`),
};

/** Whether CI=… marks a CI run (any value but "", "0" or "false"). */
export function isCI(env = process.env) {
  const v = String(env.CI ?? "").trim().toLowerCase();
  return v !== "" && v !== "0" && v !== "false";
}

/**
 * Whether generated secrets may be shown: only on an interactive terminal,
 * never in CI, where logs are kept and often shared.
 */
export function revealSecrets(env = process.env, isTTY = Boolean(process.stdout.isTTY)) {
  return isTTY && !isCI(env);
}

/** A recognizable but useless form of a secret: "byd_…wxyz". */
export function maskSecret(s) {
  const v = String(s ?? "");
  const prefix = /^[a-z]{2,8}_/.exec(v)?.[0] ?? "";
  return prefix + "…" + (v.length >= 24 ? v.slice(-4) : "");
}

/**
 * Whether prompts may run: stdin and stdout are terminals, outside CI, and
 * --non-interactive was not given.
 * @param {{nonInteractive?: boolean, env?: NodeJS.ProcessEnv, stdin?: {isTTY?: boolean}, stdout?: {isTTY?: boolean}}} [o]
 */
export function interactive(o = {}) {
  const stdin = o.stdin ?? process.stdin;
  const stdout = o.stdout ?? process.stdout;
  return Boolean(stdin.isTTY) && Boolean(stdout.isTTY) && !isCI(o.env ?? process.env) && !o.nonInteractive;
}

/**
 * @typedef {object} Prompt
 * @property {(question: string, o?: {default?: string, flag?: string}) => Promise<string>} ask
 *   One line of input (trimmed; the default when empty). Non-interactive: exit 2 naming `flag`.
 * @property {(question: string, o?: {honorYes?: boolean}) => Promise<boolean>} confirm
 *   [y/N], default No. --yes answers it unless honorYes is false. Non-interactive
 *   without --yes: exit 2 "needs confirmation: re-run with --yes".
 * @property {(question: string, items: {label: string, value: any}[], o?: {flag?: string}) => Promise<any>} choose
 *   A numbered choice; returns the value of the chosen item. Non-interactive: exit 2 naming `flag`.
 * @property {(question: string, o?: {flag?: string}) => Promise<string>} askHidden
 *   Input without echo (raw mode; Ctrl-C exits 130), e.g. an API token. Non-interactive: exit 2.
 */

/**
 * The prompts of one command run.
 * @param {{interactive: boolean, yes?: boolean, input?: NodeJS.ReadStream, output?: NodeJS.WriteStream}} o
 * @returns {Prompt}
 */
export function createPrompt(o) {
  const input = o.input ?? process.stdin;
  const output = o.output ?? process.stdout;
  const needInput = (question, flag) =>
    new CliError(
      EXIT.USAGE,
      `input needed (${String(question).trim().replace(/[:?]\s*$/, "")})${flag ? `: pass ${flag}` : ""}, or run on an interactive terminal`,
    );

  const line = (question) =>
    new Promise((resolveLine, reject) => {
      const rl = createInterface({ input, output, terminal: Boolean(input.isTTY) });
      let answered = false;
      rl.on("SIGINT", () => {
        answered = true;
        reject(new CliError(EXIT.INTERRUPTED, "interrupted"));
        rl.close();
      });
      rl.on("close", () => {
        if (!answered) reject(new CliError(EXIT.USAGE, "no answer (end of input)"));
      });
      rl.question(question, (answer) => {
        answered = true;
        rl.close();
        resolveLine(answer.trim());
      });
    });

  return {
    async ask(question, { default: def = "", flag = "" } = {}) {
      if (!o.interactive) throw needInput(question, flag);
      const answer = await line(def ? `${question} ${dim(`[${def}]`)} ` : `${question} `);
      return answer || def;
    },

    async confirm(question, { honorYes = true } = {}) {
      if (o.yes && honorYes) {
        output.write(`${question} ${dim("yes (--yes)")}\n`);
        return true;
      }
      if (!o.interactive) {
        throw new CliError(
          EXIT.USAGE,
          honorYes
            ? `needs confirmation: re-run with --yes (${String(question).trim()})`
            : `needs confirmation on an interactive terminal (${String(question).trim()})`,
        );
      }
      const answer = await line(`${question} ${dim("[y/N]")} `);
      return /^y(es)?$/i.test(answer);
    },

    async choose(question, items, { flag = "" } = {}) {
      if (!o.interactive) throw needInput(question, flag);
      items.forEach((it, i) => output.write(`  ${bold(String(i + 1))}. ${it.label}\n`));
      for (;;) {
        const answer = await line(`${question} ${dim(`[1-${items.length}]`)} `);
        const i = Number(answer) - 1;
        if (Number.isInteger(i) && items[i]) return items[i].value;
        output.write(yellow("  Please enter a number from the list.\n"));
      }
    },

    async askHidden(question, { flag = "" } = {}) {
      if (!o.interactive || typeof input.setRawMode !== "function") throw needInput(question, flag);
      return new Promise((resolveHidden, reject) => {
        output.write(`${question} `);
        const wasRaw = Boolean(input.isRaw);
        let value = "";
        const done = () => {
          input.removeListener("data", onData);
          input.setRawMode(wasRaw);
          input.pause();
          output.write("\n");
        };
        /** @param {string} chunk */
        const onData = (chunk) => {
          for (const ch of String(chunk)) {
            if (ch === "\u0003") {
              done();
              reject(new CliError(EXIT.INTERRUPTED, "interrupted"));
              return;
            }
            if (ch === "\r" || ch === "\n" || ch === "\u0004") {
              done();
              resolveHidden(value.trim());
              return;
            }
            if (ch === "\u007f" || ch === "\b") value = value.slice(0, -1);
            else value += ch;
          }
        };
        input.setRawMode(true);
        input.setEncoding("utf8");
        input.on("data", onData);
        input.resume();
      });
    },
  };
}
