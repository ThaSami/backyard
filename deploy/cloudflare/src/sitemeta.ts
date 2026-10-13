/** SiteMeta wire shape (SPEC §7.2), in the field order of the Go server. */
import type { SiteRecord } from "./registry-do";

export interface SiteMeta {
  name: string;
  url: string;
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

/** Renders a stored site record with its public URL. */
export function siteMetaJSON(r: SiteRecord, url: string): SiteMeta {
  const m: SiteMeta = { name: r.name, url } as SiteMeta;
  if (r.title) m.title = r.title;
  if (r.description) m.description = r.description;
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
