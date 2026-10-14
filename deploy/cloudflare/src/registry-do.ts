/**
 * RegistryDO: the single platform-wide Durable Object (idFromName("registry"))
 * holding the site list with SiteMeta, the user cache behind GET /users/{email},
 * the daily guardrail counters of the free-tier edition and the grants of the
 * Claude connector (SPEC §8.4.8, §16).
 */
import { DurableObject } from "cloudflare:workers";
import type { Env } from "./env";
import { fail, internal, ok, type Result } from "./http";
import type { User } from "./identity";
import type { McpGrantRecord } from "./oauth";

/** Outcome of rotating a grant (SPEC §8.4.8). */
export type RotateOutcome = "ok" | "grace" | "reuse" | "missing";

/** Seconds within which a rotated refresh token still gets its successor (SPEC §8.4.11). */
const MCP_REFRESH_GRACE = 60;

/** Most grants listed at once. */
const MCP_MAX_GRANTS = 200;
/** Expired grants and rotation markers pruned per consent (each). */
const MCP_PRUNE_PAGE = 500;

/** A row of mcp_grants. */
interface GrantRow {
  id: string;
  user_id: string;
  data: string;
  created_at: number;
  code_exp: number;
  redeemed_at: number | null;
  exp: number;
}

const GRANT_COLUMNS = "id, user_id, data, created_at, code_exp, redeemed_at, exp";

/** A stored grant: the columns plus the JSON of the other fields. */
function grantOf(r: GrantRow): McpGrantRecord {
  const d = JSON.parse(r.data) as Omit<McpGrantRecord, "id" | "user_id" | "created_at" | "code_exp" | "redeemed_at" | "exp">;
  return { ...d, id: r.id, user_id: r.user_id, created_at: r.created_at, code_exp: r.code_exp, redeemed_at: r.redeemed_at ?? null, exp: r.exp };
}

/** SiteMeta as stored (the Worker adds the URL per request). */
export interface SiteRecord {
  name: string;
  title?: string;
  description?: string;
  created_at: string;
  created_by?: string;
  updated_at: string;
  updated_by?: string;
  deploy_id?: string;
  file_count: number;
  bytes: number;
  owners: string[];
}

/** One counter to charge: key ("ai|user:alice"), daily limit (0 = unlimited) and amount (default 1). */
export interface Charge {
  key: string;
  limit: number;
  /** Units this request uses (e.g. the number of images generated); default 1. */
  amount?: number;
}

/** The amount of a charge: a positive integer, 1 when unset. */
export function chargeAmount(c: Charge): number {
  const n = c.amount ?? 1;
  return Number.isSafeInteger(n) && n > 0 ? n : 1;
}

/** Result of charging counters. */
export interface ChargeResult {
  allowed: boolean;
  /** Key of the first exhausted counter. */
  exhausted?: string;
  /** Seconds until the counters reset (UTC midnight). */
  retryAfter?: number;
}

function utcDay(ms = Date.now()): string {
  return new Date(ms).toISOString().slice(0, 10);
}

function failure<T>(e: unknown, what: string): Result<T> {
  console.error(`backyard: registry: ${what} failed: ${(e as Error)?.message ?? e}`);
  return fail(internal(`${what} failed`));
}

export class RegistryDO extends DurableObject<Env> {
  private readonly sql: SqlStorage;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    this.sql.exec(`CREATE TABLE IF NOT EXISTS sites (name TEXT PRIMARY KEY, meta TEXT NOT NULL, updated_at TEXT NOT NULL)`);
    this.sql.exec(`CREATE TABLE IF NOT EXISTS counters (key TEXT PRIMARY KEY, day TEXT NOT NULL, n INTEGER NOT NULL)`);
    this.sql.exec(`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, email TEXT NOT NULL, data TEXT NOT NULL, updated_at INTEGER NOT NULL)`);
    this.sql.exec(`CREATE INDEX IF NOT EXISTS users_email ON users (email)`);
    // Slots of max_sites held by first deploys between their commit and putSite.
    this.sql.exec(`CREATE TABLE IF NOT EXISTS site_reservations (name TEXT PRIMARY KEY, expires_at INTEGER NOT NULL)`);
    // Claude connector (SPEC §8.4.8): one grant per consent (times in unix
    // seconds; data holds the other fields as JSON) and the markers of its
    // newest two issued refresh-token generations.
    this.sql.exec(
      `CREATE TABLE IF NOT EXISTS mcp_grants (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, data TEXT NOT NULL, created_at INTEGER NOT NULL, code_exp INTEGER NOT NULL, redeemed_at INTEGER, exp INTEGER NOT NULL)`,
    );
    this.sql.exec(`CREATE INDEX IF NOT EXISTS mcp_grants_user ON mcp_grants (user_id)`);
    this.sql.exec(`CREATE INDEX IF NOT EXISTS mcp_grants_exp ON mcp_grants (exp)`);
    this.sql.exec(`CREATE TABLE IF NOT EXISTS mcp_rotations (gid TEXT NOT NULL, n INTEGER NOT NULL, at INTEGER NOT NULL, exp INTEGER NOT NULL, PRIMARY KEY (gid, n))`);
    this.sql.exec(`CREATE INDEX IF NOT EXISTS mcp_rotations_exp ON mcp_rotations (exp)`);
  }

  /** Sites plus live reservations of names that are not sites yet. */
  private siteSlots(now: number): number {
    const sites = (this.sql.exec("SELECT COUNT(*) AS n FROM sites").one() as { n: number }).n;
    const held = (
      this.sql.exec("SELECT COUNT(*) AS n FROM site_reservations WHERE expires_at > ? AND name NOT IN (SELECT name FROM sites)", now).one() as {
        n: number;
      }
    ).n;
    return sites + held;
  }

  private siteExists(name: string): boolean {
    return this.sql.exec("SELECT 1 FROM sites WHERE name = ?", name).toArray().length > 0;
  }

  private reserved(name: string, now: number): boolean {
    return this.sql.exec("SELECT 1 FROM site_reservations WHERE name = ? AND expires_at > ?", name, now).toArray().length > 0;
  }

  /** All sites, sorted by name. */
  async listSites(): Promise<Result<SiteRecord[]>> {
    try {
      const rows = this.sql.exec("SELECT meta FROM sites ORDER BY name").toArray() as { meta: string }[];
      return ok(rows.map((r) => JSON.parse(r.meta) as SiteRecord));
    } catch (e) {
      return failure(e, "listing sites");
    }
  }

  /** One site, or null. */
  async getSite(name: string): Promise<Result<SiteRecord | null>> {
    try {
      const row = this.sql.exec("SELECT meta FROM sites WHERE name = ?", name).toArray()[0] as { meta: string } | undefined;
      return ok(row ? (JSON.parse(row.meta) as SiteRecord) : null);
    } catch (e) {
      return failure(e, "reading the site");
    }
  }

  /** Creates or replaces a site's metadata (and ends its reservation). */
  async putSite(meta: SiteRecord): Promise<Result<null>> {
    try {
      this.sql.exec("INSERT OR REPLACE INTO sites (name, meta, updated_at) VALUES (?, ?, ?)", meta.name, JSON.stringify(meta), meta.updated_at);
      this.sql.exec("DELETE FROM site_reservations WHERE name = ?", meta.name);
      return ok(null);
    } catch (e) {
      return failure(e, "saving the site");
    }
  }

  /** Removes a site; reports whether it existed. */
  async deleteSite(name: string): Promise<Result<boolean>> {
    try {
      this.sql.exec("DELETE FROM site_reservations WHERE name = ?", name);
      return ok(this.sql.exec("DELETE FROM sites WHERE name = ?", name).rowsWritten > 0);
    } catch (e) {
      return failure(e, "deleting the site");
    }
  }

  /**
   * Checks the platform-wide site cap for a deploy to name: allowed when the
   * site exists (or is reserved) or fewer than maxSites sites and
   * reservations exist (0 = unlimited). Only a fast path: reserveSite decides.
   */
  async canCreateSite(name: string, maxSites: number): Promise<Result<{ allowed: boolean; count: number }>> {
    try {
      const now = Date.now();
      const count = this.siteSlots(now);
      return ok({ allowed: maxSites <= 0 || this.siteExists(name) || this.reserved(name, now) || count < maxSites, count });
    } catch (e) {
      return failure(e, "checking the site cap");
    }
  }

  /**
   * Takes one of the platform's maxSites slots for a new site before its first
   * commit, until putSite records the site (or ttlMs passes, or releaseSite):
   * the check and the reservation run without an await in between, so
   * concurrent first deploys can never exceed the cap (0 = unlimited). An
   * existing or already reserved name needs no new slot.
   */
  async reserveSite(name: string, maxSites: number, ttlMs: number): Promise<Result<{ allowed: boolean }>> {
    try {
      if (maxSites <= 0 || this.siteExists(name)) return ok({ allowed: true });
      const now = Date.now();
      this.sql.exec("DELETE FROM site_reservations WHERE expires_at <= ?", now);
      if (!this.reserved(name, now) && this.siteSlots(now) >= maxSites) return ok({ allowed: false });
      this.sql.exec("INSERT OR REPLACE INTO site_reservations (name, expires_at) VALUES (?, ?)", name, now + Math.max(1000, ttlMs));
      return ok({ allowed: true });
    } catch (e) {
      return failure(e, "checking the site cap");
    }
  }

  /** Gives a reservation back (a first commit that failed). */
  async releaseSite(name: string): Promise<Result<null>> {
    try {
      this.sql.exec("DELETE FROM site_reservations WHERE name = ?", name);
      return ok(null);
    } catch (e) {
      return failure(e, "releasing the site slot");
    }
  }

  /**
   * Charges every counter by its amount for today (UTC) if none would exceed
   * its limit; otherwise charges nothing and reports the exhausted counter.
   * Check and update run without an await in between, so concurrent charges
   * can never overshoot a limit.
   */
  async charge(charges: Charge[]): Promise<Result<ChargeResult>> {
    try {
      const now = Date.now();
      const day = utcDay(now);
      const current = charges.map((c) => {
        const row = this.sql.exec("SELECT day, n FROM counters WHERE key = ?", c.key).toArray()[0] as { day: string; n: number } | undefined;
        return row && row.day === day ? row.n : 0;
      });
      for (let i = 0; i < charges.length; i++) {
        const c = charges[i]!;
        if (c.limit > 0 && current[i]! + chargeAmount(c) > c.limit) {
          const d = new Date(now);
          const next = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() + 1);
          return ok({ allowed: false, exhausted: c.key, retryAfter: Math.max(1, Math.ceil((next - now) / 1000)) });
        }
      }
      this.ctx.storage.transactionSync(() => {
        for (let i = 0; i < charges.length; i++) {
          this.sql.exec("INSERT OR REPLACE INTO counters (key, day, n) VALUES (?, ?, ?)", charges[i]!.key, day, current[i]! + chargeAmount(charges[i]!));
        }
      });
      if (Math.random() < 0.01) this.sql.exec("DELETE FROM counters WHERE day < ?", day);
      return ok({ allowed: true });
    } catch (e) {
      return failure(e, "charging counters");
    }
  }

  /** Today's value of counters (for tests and diagnostics). */
  async counters(keys: string[]): Promise<Result<Record<string, number>>> {
    try {
      const day = utcDay();
      const out: Record<string, number> = {};
      for (const k of keys) {
        const row = this.sql.exec("SELECT day, n FROM counters WHERE key = ?", k).toArray()[0] as { day: string; n: number } | undefined;
        out[k] = row && row.day === day ? row.n : 0;
      }
      return ok(out);
    } catch (e) {
      return failure(e, "reading counters");
    }
  }

  /** Records a user in the user cache (GET /users/{email}). */
  async touchUser(u: User): Promise<Result<null>> {
    try {
      if (!u.email) return ok(null);
      this.sql.exec(
        "INSERT OR REPLACE INTO users (id, email, data, updated_at) VALUES (?, ?, ?, ?)",
        u.id,
        u.email.toLowerCase(),
        JSON.stringify({ ...u, admin: false }),
        Date.now(),
      );
      return ok(null);
    } catch (e) {
      return failure(e, "recording the user");
    }
  }

  /** Looks a user up by email. */
  async lookupUser(email: string): Promise<Result<User | null>> {
    try {
      const row = this.sql.exec("SELECT data FROM users WHERE email = ? ORDER BY updated_at DESC LIMIT 1", email.toLowerCase()).toArray()[0] as
        | { data: string }
        | undefined;
      return ok(row ? (JSON.parse(row.data) as User) : null);
    } catch (e) {
      return failure(e, "looking up the user");
    }
  }

  /** Liveness check for /readyz. */
  async ping(): Promise<Result<string>> {
    return ok("ok");
  }

  // --- Claude connector grants (SPEC §8.4.8) ---------------------------------

  private grantRow(id: string): GrantRow | undefined {
    return this.sql.exec(`SELECT ${GRANT_COLUMNS} FROM mcp_grants WHERE id = ?`, id).toArray()[0] as GrantRow | undefined;
  }

  private dropGrant(id: string): void {
    this.sql.exec("DELETE FROM mcp_rotations WHERE gid = ?", id);
    this.sql.exec("DELETE FROM mcp_grants WHERE id = ?", id);
  }

  /** Stores a new (pending) grant, after pruning expired grants and rotation markers. */
  async mcpGrantCreate(g: McpGrantRecord): Promise<Result<null>> {
    try {
      const now = Math.floor(Date.now() / 1000);
      const { id, user_id, created_at, code_exp, redeemed_at, exp, ...rest } = g;
      this.ctx.storage.transactionSync(() => {
        // Bounded, over the exp indexes: a backlog is worked off by later consents.
        this.sql.exec(`DELETE FROM mcp_grants WHERE id IN (SELECT id FROM mcp_grants WHERE exp < ? LIMIT ${MCP_PRUNE_PAGE})`, now);
        this.sql.exec(`DELETE FROM mcp_rotations WHERE rowid IN (SELECT rowid FROM mcp_rotations WHERE exp < ? LIMIT ${MCP_PRUNE_PAGE})`, now);
        this.sql.exec(
          `INSERT INTO mcp_grants (${GRANT_COLUMNS}) VALUES (?, ?, ?, ?, ?, ?, ?)`,
          id,
          user_id,
          JSON.stringify(rest),
          created_at,
          code_exp,
          redeemed_at ?? null,
          exp,
        );
      });
      return ok(null);
    } catch (e) {
      return failure(e, "saving the connection");
    }
  }

  /** One grant (pending, redeemed or expired), or null. */
  async mcpGrantGet(id: string): Promise<Result<McpGrantRecord | null>> {
    try {
      const r = this.grantRow(id);
      return ok(r ? grantOf(r) : null);
    } catch (e) {
      return failure(e, "reading the connection");
    }
  }

  /**
   * rotate (SPEC §8.4.8), the only state transition of a grant, in one
   * transaction: code redemption calls it with from=0, grace=false (and the
   * grant's family expiry, which becomes its expiry); refreshing generation n
   * with from=n, grace=true. now is unix seconds. Only the markers of the
   * newest two generations are kept: a presented generation without its
   * marker is an old one, and a redeemed grant proves a replayed code.
   */
  async mcpRotate(gid: string, from: number, now: number, grace: boolean, familyExp: number): Promise<Result<RotateOutcome>> {
    try {
      const outcome = this.ctx.storage.transactionSync((): RotateOutcome => {
        const g = this.grantRow(gid);
        if (!g || g.exp <= now) return "missing";
        if (from >= 1 && g.redeemed_at === null) return "missing";
        const reuse = (): RotateOutcome => {
          this.dropGrant(gid);
          return "reuse";
        };
        if (from === 0 && g.redeemed_at !== null) return reuse(); // a replayed code
        if (from >= 1 && this.sql.exec("SELECT 1 FROM mcp_rotations WHERE gid = ? AND n = ?", gid, from).toArray().length === 0) return reuse(); // compacted away
        if (from >= 2) this.sql.exec("DELETE FROM mcp_rotations WHERE gid = ? AND n = ?", gid, from - 1);
        const created = this.sql.exec("INSERT OR IGNORE INTO mcp_rotations (gid, n, at, exp) VALUES (?, ?, ?, ?)", gid, from + 1, now, familyExp);
        if (created.rowsWritten > 0) {
          if (from === 0) this.sql.exec("UPDATE mcp_grants SET redeemed_at = ?, exp = ? WHERE id = ?", now, familyExp, gid);
          return "ok";
        }
        if (grace) {
          const m = this.sql.exec("SELECT at FROM mcp_rotations WHERE gid = ? AND n = ?", gid, from + 1).toArray()[0] as { at: number } | undefined;
          const next = this.sql.exec("SELECT 1 FROM mcp_rotations WHERE gid = ? AND n = ?", gid, from + 2).toArray().length > 0;
          if (m && now - m.at <= MCP_REFRESH_GRACE && !next) return "grace";
        }
        // A replay of a code, or an old refresh generation: revoke the grant.
        return reuse();
      });
      return ok(outcome);
    } catch (e) {
      return failure(e, "updating the connection");
    }
  }

  /** Deletes a grant and its rotation markers; returns the deleted grant, or null. */
  async mcpGrantRevoke(id: string): Promise<Result<McpGrantRecord | null>> {
    try {
      const g = this.ctx.storage.transactionSync(() => {
        const r = this.grantRow(id);
        if (r) this.dropGrant(id);
        return r;
      });
      return ok(g ? grantOf(g) : null);
    } catch (e) {
      return failure(e, "revoking the connection");
    }
  }

  /** The live (redeemed, unexpired) grants of a user, newest first (at most limit, ≤ 200). */
  async mcpGrantsByUser(userId: string, limit: number): Promise<Result<McpGrantRecord[]>> {
    try {
      const now = Math.floor(Date.now() / 1000);
      const n = Math.max(1, Math.min(MCP_MAX_GRANTS, Math.floor(limit) || MCP_MAX_GRANTS));
      const rows = this.sql
        .exec(
          `SELECT ${GRANT_COLUMNS} FROM mcp_grants WHERE user_id = ? AND redeemed_at IS NOT NULL AND exp > ? ORDER BY created_at DESC, id DESC LIMIT ?`,
          userId,
          now,
          n,
        )
        .toArray() as unknown as GrantRow[];
      return ok(rows.map(grantOf));
    } catch (e) {
      return failure(e, "listing connections");
    }
  }

  /** Deletes every grant of a user (pending ones included); returns how many there were. */
  async mcpRevokeUser(userId: string): Promise<Result<number>> {
    try {
      const n = this.ctx.storage.transactionSync(() => {
        const count = (this.sql.exec("SELECT COUNT(*) AS n FROM mcp_grants WHERE user_id = ?", userId).one() as { n: number }).n;
        this.sql.exec("DELETE FROM mcp_rotations WHERE gid IN (SELECT id FROM mcp_grants WHERE user_id = ?)", userId);
        this.sql.exec("DELETE FROM mcp_grants WHERE user_id = ?", userId);
        return count;
      });
      return ok(n);
    } catch (e) {
      return failure(e, "revoking connections");
    }
  }
}
