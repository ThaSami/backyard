// Command-line parser and help of the engine, both generated from
// commands.json (the single source of the engine's CLI surface, also read by
// the Go CLI's help parity test and the docs test).
//
// Accepted forms: `--flag value`, `--flag=value`, repeatable flags, the short
// flags `-y` and `-h`, and the positional arguments a command declares. An
// unknown flag, a missing value or an extra positional exits 2 with the
// command's usage. Options come back camelCased: --worker-name → workerName,
// --no-r2 → noR2, --var → var (an array, as every repeatable flag), --wait →
// a number (validated: a whole number of seconds from 0 to 3600).

import { readFileSync } from "node:fs";

import { CliError, EXIT } from "./ui.mjs";

/**
 * @typedef {object} FlagSpec
 * @property {string} name
 * @property {"bool"|"string"} type
 * @property {string[]} [alias]
 * @property {string} [short]
 * @property {string} [value]       placeholder shown in the help (`--zone <zone>`)
 * @property {boolean} [repeatable]
 * @property {string} help
 *
 * @typedef {object} CommandSpec
 * @property {string} name          "setup", "domain add", …
 * @property {string[]} args        positional argument names (`<host>`)
 * @property {string} summary
 * @property {FlagSpec[]} flags
 *
 * @typedef {{schema: number, global: FlagSpec[], commands: CommandSpec[]}} CommandsFile
 */

/** @type {CommandsFile} */
export const COMMANDS = JSON.parse(readFileSync(new URL("./commands.json", import.meta.url), "utf8"));

/** --worker-name → workerName */
export const camel = (name) => String(name).replace(/-([a-z0-9])/g, (_, c) => c.toUpperCase());

const WAIT_MAX = 3600;

/**
 * @param {string} name
 * @param {CommandsFile} [spec]
 * @returns {CommandSpec | undefined}
 */
export function findCommand(name, spec = COMMANDS) {
  return spec.commands.find((c) => c.name === name);
}

/** The command groups ("domain", "access") and their subcommands. */
function groups(spec) {
  const g = new Map();
  for (const c of spec.commands) {
    const [first, second] = c.name.split(" ");
    if (second) g.set(first, [...(g.get(first) ?? []), c.name]);
  }
  return g;
}

/**
 * @typedef {object} Parsed
 * @property {"run"|"help"|"version"} kind
 * @property {string} command    the command ("" for the general help)
 * @property {string} [group]    "domain"/"access" for a group's help
 * @property {Record<string, any>} opts   camelCased flags and positionals
 * @property {number} exitCode   for kind "help": 0 when asked for, 2 when no command was given
 */

/**
 * Parses argv (without node and the script). Throws CliError(2) with the
 * usage for anything invalid.
 * @param {string[]} argv
 * @param {{invokedAs?: string, spec?: CommandsFile}} [o]
 * @returns {Parsed}
 */
export function parseArgs(argv, o = {}) {
  const spec = o.spec ?? COMMANDS;
  const invokedAs = o.invokedAs ?? "backyard-cloudflare";
  const usageError = (msg, command = "", group = "") =>
    new CliError(EXIT.USAGE, `${msg}\n\n${formatHelp(command, { invokedAs, spec, group })}`, { reason: "usage" });

  if (argv.length === 0) return { kind: "help", command: "", opts: {}, exitCode: EXIT.USAGE };
  const [a0, a1] = argv;
  if (a0 === "--version") {
    if (argv.length > 1) throw usageError(`--version takes no arguments`);
    return { kind: "version", command: "", opts: {}, exitCode: EXIT.OK };
  }
  if (a0 === "-h" || a0 === "--help") return { kind: "help", command: "", opts: {}, exitCode: EXIT.OK };
  const g = groups(spec);
  if (a0 === "help") {
    const rest = argv.slice(1).join(" ");
    if (!rest) return { kind: "help", command: "", opts: {}, exitCode: EXIT.OK };
    if (findCommand(rest, spec)) return { kind: "help", command: rest, opts: {}, exitCode: EXIT.OK };
    if (g.has(rest)) return { kind: "help", command: "", group: rest, opts: {}, exitCode: EXIT.OK };
    throw usageError(`unknown command ${JSON.stringify(rest)}`);
  }

  let command;
  let rest;
  if (g.has(a0)) {
    const name = a1 !== undefined && !a1.startsWith("-") ? `${a0} ${a1}` : "";
    command = name ? findCommand(name, spec) : undefined;
    if (!command) {
      if (a1 === "-h" || a1 === "--help") return { kind: "help", command: "", group: a0, opts: {}, exitCode: EXIT.OK };
      const subs = g.get(a0).join(", ");
      throw usageError(a1 === undefined || a1.startsWith("-") ? `${a0} needs a subcommand: ${subs}` : `unknown command ${JSON.stringify(`${a0} ${a1}`)} (try ${subs})`, "", a0);
    }
    rest = argv.slice(2);
  } else {
    command = findCommand(a0, spec);
    if (!command || command.name.includes(" ")) throw usageError(`unknown command ${JSON.stringify(a0)}`);
    rest = argv.slice(1);
  }

  /** @type {Map<string, FlagSpec>} */
  const byToken = new Map();
  const flags = [...command.flags, ...spec.global];
  for (const f of flags) {
    byToken.set(`--${f.name}`, f);
    for (const al of f.alias ?? []) byToken.set(`--${al}`, f);
    if (f.short) byToken.set(`-${f.short}`, f);
  }
  /** @type {Record<string, any>} */
  const opts = {};
  for (const f of flags) opts[camel(f.name)] = f.type === "bool" ? false : f.repeatable ? [] : undefined;

  // Help wins over every other problem of the line (`setup --bogus --help`).
  const dd = rest.indexOf("--");
  const optionPart = dd >= 0 ? rest.slice(0, dd) : rest;
  if (optionPart.some((t) => t === "-h" || t === "--help")) return { kind: "help", command: command.name, opts, exitCode: EXIT.OK };

  const positionals = [];
  for (let i = 0; i < rest.length; i++) {
    const tok = rest[i];
    if (tok === "--") {
      positionals.push(...rest.slice(i + 1));
      break;
    }
    if (!tok.startsWith("-") || tok === "-") {
      positionals.push(tok);
      continue;
    }
    const eq = tok.startsWith("--") ? tok.indexOf("=") : -1;
    const key = eq >= 0 ? tok.slice(0, eq) : tok;
    const f = byToken.get(key);
    if (!f) throw usageError(`unknown option ${key}`, command.name);
    const name = camel(f.name);
    if (f.type === "bool") {
      if (eq >= 0) throw usageError(`${key} takes no value`, command.name);
      opts[name] = true;
      continue;
    }
    let value;
    if (eq >= 0) value = tok.slice(eq + 1);
    else {
      value = rest[++i];
      if (value === undefined) throw usageError(`${key} needs a value`, command.name);
    }
    if (f.name === "wait") {
      if (!/^\d+$/.test(value) || Number(value) > WAIT_MAX) {
        throw usageError(`--wait must be a whole number of seconds from 0 to ${WAIT_MAX} (got ${JSON.stringify(value)})`, command.name);
      }
      value = Number(value);
    }
    if (f.repeatable) opts[name].push(value);
    else opts[name] = value;
  }
  if (positionals.length > command.args.length) {
    throw usageError(`unexpected argument ${JSON.stringify(positionals[command.args.length])}`, command.name);
  }
  for (const [i, arg] of command.args.entries()) {
    if (positionals[i] === undefined || positionals[i] === "") throw usageError(`${command.name} needs <${arg}>`, command.name);
    opts[camel(arg)] = positionals[i];
  }
  return { kind: "run", command: command.name, opts, exitCode: EXIT.OK };
}

/** `--zone <zone>`, `-y, --yes` */
function flagLabel(f) {
  const long = `--${f.name}${f.type === "string" ? ` <${f.value ?? "value"}>` : ""}`;
  return f.short ? `-${f.short}, ${long}` : long;
}

function flagHelp(f) {
  const extra = [];
  if (f.alias?.length) extra.push(`alias ${f.alias.map((a) => `--${a}`).join(", ")}`);
  if (f.repeatable) extra.push("repeatable");
  return extra.length ? `${f.help} (${extra.join("; ")})` : f.help;
}

/** Two columns; `all` are every row that should share the column width. */
function table(rows, all = rows) {
  const width = Math.min(30, Math.max(...all.map(([l]) => l.length)) + 2);
  return rows.map(([l, r]) => (l.length + 2 > width ? `  ${l}\n  ${" ".repeat(width)}${r}` : `  ${l.padEnd(width)}${r}`)).join("\n");
}

const EXIT_CODES =
  "Exit codes: 0 done · 1 failed (a re-run resumes) · 2 usage, or input needed · 3 refused for safety (nothing changed) · 4 applied, verification pending (a re-run resumes) · 130 interrupted";

/**
 * The help of one command, of a group ("domain") or, without either, the
 * general help.
 * @param {string} [commandName]
 * @param {{invokedAs?: string, spec?: CommandsFile, group?: string}} [o]
 */
export function formatHelp(commandName = "", o = {}) {
  const spec = o.spec ?? COMMANDS;
  const inv = o.invokedAs ?? "backyard-cloudflare";
  const globalRows = spec.global.map((f) => [flagLabel(f), flagHelp(f)]);
  const command = commandName ? findCommand(commandName, spec) : undefined;
  if (command) {
    const args = command.args.map((a) => ` <${a}>`).join("");
    const lines = [`Usage: ${inv} ${command.name}${args} [options]`, "", `${command.summary}.`, ""];
    const rows = command.flags.map((f) => [flagLabel(f), flagHelp(f)]);
    if (rows.length) lines.push("Options:", table(rows, [...rows, ...globalRows]), "");
    lines.push("Global options:", table(globalRows, [...rows, ...globalRows]), "");
    if (command.name === "setup") {
      lines.push(
        `Afterwards: your own domain with \`${inv} domain add <host>\`, company login with`,
        `\`${inv} access enable --allow-email-domain <your-domain>\`, a live check with \`${inv} status\`.`,
        "",
      );
    }
    lines.push(EXIT_CODES);
    return lines.join("\n") + "\n";
  }
  const cmds = spec.commands.filter((c) => !o.group || c.name.startsWith(`${o.group} `));
  const rows = cmds.map((c) => [`${c.name}${c.args.map((a) => ` <${a}>`).join("")}`, c.summary]);
  const lines = [
    `Usage: ${inv} ${o.group ? `${o.group} <subcommand>` : "<command>"} [options]`,
    "",
    "Backyard on your own Cloudflare account (Workers free plan): setup, custom domain and",
    "Cloudflare Access, without Terraform.",
    "",
    "Commands:",
    table(rows),
    "",
    "Global options:",
    table([...globalRows, ["--version", "print the version (backyard-cloudflare <version> (wrangler <version>))"]], [...rows, ...globalRows]),
    "",
    `Run \`${inv} <command> --help\` for the options of a command.`,
    EXIT_CODES,
  ];
  return lines.join("\n") + "\n";
}
