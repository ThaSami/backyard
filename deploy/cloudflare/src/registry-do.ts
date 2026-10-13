/**
 * RegistryDO: the single platform-wide Durable Object (idFromName("registry"))
 * holding the site list with SiteMeta, the user cache behind GET /users/{email}
 * and the daily guardrail counters of the free-tier edition (SPEC §16).
 */
import { DurableObject } from "cloudflare:workers";
import type { Env } from "./env";
import { fail, internal, ok, type Result } from "./http";
import type { User } from "./identity";

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

/** One counter to charge: key ("ai|user:alice") and daily limit (0 = unlimited). */
export interface Charge {
  key: string;
  limit: number;
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

  /** Creates or replaces a site's metadata. */
  async putSite(meta: SiteRecord): Promise<Result<null>> {
    try {
      this.sql.exec("INSERT OR REPLACE INTO sites (name, meta, updated_at) VALUES (?, ?, ?)", meta.name, JSON.stringify(meta), meta.updated_at);
      return ok(null);
    } catch (e) {
      return failure(e, "saving the site");
    }
  }

  /** Removes a site; reports whether it existed. */
  async deleteSite(name: string): Promise<Result<boolean>> {
    try {
      return ok(this.sql.exec("DELETE FROM sites WHERE name = ?", name).rowsWritten > 0);
    } catch (e) {
      return failure(e, "deleting the site");
    }
  }

  /**
   * Checks the platform-wide site cap for a deploy to name: allowed when the
   * site exists or fewer than maxSites sites exist (0 = unlimited).
   */
  async canCreateSite(name: string, maxSites: number): Promise<Result<{ allowed: boolean; count: number }>> {
    try {
      const exists = this.sql.exec("SELECT 1 FROM sites WHERE name = ?", name).toArray().length > 0;
      const count = (this.sql.exec("SELECT COUNT(*) AS n FROM sites").one() as { n: number }).n;
      return ok({ allowed: exists || maxSites <= 0 || count < maxSites, count });
    } catch (e) {
      return failure(e, "checking the site cap");
    }
  }

  /**
   * Charges every counter by one for today (UTC) if none would exceed its
   * limit; otherwise charges nothing and reports the exhausted counter.
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
        if (c.limit > 0 && current[i]! >= c.limit) {
          const d = new Date(now);
          const next = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() + 1);
          return ok({ allowed: false, exhausted: c.key, retryAfter: Math.max(1, Math.ceil((next - now) / 1000)) });
        }
      }
      this.ctx.storage.transactionSync(() => {
        for (let i = 0; i < charges.length; i++) {
          this.sql.exec("INSERT OR REPLACE INTO counters (key, day, n) VALUES (?, ?, ?)", charges[i]!.key, day, current[i]! + 1);
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
}
