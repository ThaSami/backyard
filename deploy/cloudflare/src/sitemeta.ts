/** SiteMeta wire shape (SPEC §7.2), in the field order of the Go server, and who sees a site listed. */
import { isAdminIdentity, type Config } from "./config";
import type { User } from "./identity";
import type { SiteRecord } from "./registry-do";

export interface SiteMeta {
  name: string;
  url: string;
  title?: string;
  description?: string;
  preview?: string;
  created_at: string;
  created_by?: string;
  updated_at: string;
  updated_by?: string;
  deploy_id?: string;
  file_count: number;
  bytes: number;
  owners: string[];
}

/** Renders a stored site record with its public URL. */
export function siteMetaJSON(r: SiteRecord, url: string): SiteMeta {
  const m: SiteMeta = { name: r.name, url } as SiteMeta;
  if (r.title) m.title = r.title;
  if (r.description) m.description = r.description;
  if (r.preview) m.preview = r.preview;
  m.created_at = r.created_at;
  if (r.created_by) m.created_by = r.created_by;
  m.updated_at = r.updated_at;
  if (r.updated_by) m.updated_by = r.updated_by;
  if (r.deploy_id) m.deploy_id = r.deploy_id;
  m.file_count = r.file_count;
  m.bytes = r.bytes;
  m.owners = r.owners ?? [];
  return m;
}

/** Whether id or email is listed as an owner (case-insensitive). */
export function isOwner(r: SiteRecord, id: string, email: string): boolean {
  return (r.owners ?? []).some((o) => o.toLowerCase() === id.toLowerCase() || (email !== "" && o.toLowerCase() === email.toLowerCase()));
}

/**
 * Whether r appears in u's site listings (SITE_LISTING, SPEC §7.2 "Listing";
 * the Go server's sites.Listed): the hub's /__backyard/api/sites and its
 * preview route, the gateway's GET /sites and backyard_list_sites. With
 * "all" every site does. With "owners" a site is listed only to a caller who
 * is not anonymous and is an admin (the gateway's rule: u.admin, or ADMINS /
 * ADMIN_GROUPS), one of the site's owners (by id or email, case-insensitive),
 * or, while the site has no owners, the one who created it (created_by, by id
 * or email, case-insensitive), so that sites from before the option stay
 * listed to their creator. Listing is not access control: lookups by name
 * are not filtered.
 */
export function listedTo(cfg: Config, u: User | null | undefined, r: SiteRecord | null | undefined): boolean {
  if (cfg.siteListing !== "owners") return true;
  if (!u || u.anonymous || !r) return false;
  const email = u.email ?? "";
  if (u.admin || isAdminIdentity(cfg, u.id, email, u.groups ?? [])) return true;
  if (r.owners?.length) return isOwner(r, u.id, email);
  const c = (r.created_by ?? "").toLowerCase();
  return c !== "" && ((u.id !== "" && c === u.id.toLowerCase()) || (email !== "" && c === email.toLowerCase()));
}

/** The records of list that are listed to u (listedTo), in order; list itself when every site is. */
export function filterListed(cfg: Config, u: User | null | undefined, list: SiteRecord[]): SiteRecord[] {
  return cfg.siteListing === "owners" ? list.filter((r) => listedTo(cfg, u, r)) : list;
}
