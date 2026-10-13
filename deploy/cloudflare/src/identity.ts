/** The User shape of SPEC §4 (returned by /me, embedded in presence). */
export interface User {
  id: string;
  email?: string;
  name: string;
  title?: string;
  team?: string;
  slack?: string;
  avatar_url?: string;
  groups?: string[];
  anonymous: boolean;
  admin: boolean;
  extra?: Record<string, unknown>;
}

/** Compact user embedded in realtime frames (SPEC §6). */
export interface UserSummary {
  id: string;
  name: string;
  email?: string;
  avatar_url?: string;
}

export function summarize(u: User): UserSummary {
  const s: UserSummary = { id: u.id, name: u.name };
  if (u.email) s.email = u.email;
  if (u.avatar_url) s.avatar_url = u.avatar_url;
  return s;
}

/** Identifier recorded in created_by / updated_by. */
export function actorOf(u: User | null | undefined): string {
  return u && u.id ? u.id : "unknown";
}

/** Normalizes an email (trim + lowercase). */
export function normalizeEmail(s: string): string {
  return s.trim().toLowerCase();
}

/** Returns a JSON-safe copy of u with empty optional fields removed (Go omitempty). */
export function cleanUser(u: User): User {
  const out: User = { id: u.id, name: u.name || u.id, anonymous: !!u.anonymous, admin: !!u.admin };
  if (u.email) out.email = u.email;
  if (u.title) out.title = u.title;
  if (u.team) out.team = u.team;
  if (u.slack) out.slack = u.slack;
  if (u.avatar_url) out.avatar_url = u.avatar_url;
  if (u.groups && u.groups.length) out.groups = [...u.groups];
  if (u.extra && Object.keys(u.extra).length) out.extra = { ...u.extra };
  // Key order of the Go struct: id, email, name, title, team, slack, avatar_url, groups, anonymous, admin, extra.
  const ordered: Record<string, unknown> = {};
  for (const k of ["id", "email", "name", "title", "team", "slack", "avatar_url", "groups", "anonymous", "admin", "extra"]) {
    if (k in out) ordered[k] = (out as unknown as Record<string, unknown>)[k];
  }
  return ordered as unknown as User;
}
