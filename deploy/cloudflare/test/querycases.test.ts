/**
 * The shared query-semantics suite of SPEC §5.2
 * (internal/docstore/conformance/querycases.json, copied to
 * test/generated/querycases.json by scripts/sync-assets.mjs), run against the
 * Worker's query engine over HTTP: POST /db/{c} creates the fixtures in the
 * listed order, POST /db/{c}/query runs every case. The Go backends and the
 * black-box conformance suite run the very same file.
 */
import { beforeAll, describe, expect, it } from "vitest";
import raw from "./generated/querycases.json";
import { apiHeaders, call, uniqueSite } from "./helpers";

interface QueryCases {
  version: number;
  /** Set by sync-assets when the source file could not be read. */
  missing?: string;
  collections: Record<string, { id: string; data: Record<string, unknown> }[]>;
  cases: { name: string; collection: string; query: Record<string, unknown>; want: string[] | null; has_more: boolean }[];
}

const qc = raw as unknown as QueryCases;

describe("shared query cases (querycases.json)", () => {
  const site = uniqueSite("qc");
  const api = (path: string) => `/s/${site}/__backyard/api${path}`;

  it("are present (run npm run sync in a complete checkout)", () => {
    expect(qc.missing, `scripts/sync-assets.mjs could not read ${qc.missing ?? ""}; set BACKYARD_REPO_ROOT to a Backyard checkout`).toBeUndefined();
    expect(qc.version).toBeGreaterThanOrEqual(1);
    expect(qc.cases.length).toBeGreaterThan(0);
    for (const c of qc.cases) expect(qc.collections[c.collection], `${c.name}: unknown collection ${c.collection}`).toBeDefined();
  });

  beforeAll(async () => {
    // Collections in name order, documents in the listed order (creation
    // order defines _meta.created_at, the default sort key).
    for (const name of Object.keys(qc.collections).sort()) {
      for (const d of qc.collections[name]!) {
        const res = await call(api(`/db/${name}`), { method: "POST", headers: apiHeaders(), body: JSON.stringify({ ...d.data, id: d.id }) });
        if (res.status !== 201) throw new Error(`creating ${name}/${d.id}: ${res.status} ${await res.text()}`);
      }
    }
  });

  it.each(qc.cases.map((c) => [c.name, c] as const))("%s", async (_name, c) => {
    const res = await call(api(`/db/${c.collection}/query`), { method: "POST", headers: apiHeaders(), body: JSON.stringify(c.query) });
    expect(res.status, await res.clone().text()).toBe(200);
    const page = (await res.json()) as { docs: { id: string }[]; has_more: boolean };
    expect({ ids: page.docs.map((d) => d.id), has_more: page.has_more }).toEqual({ ids: c.want ?? [], has_more: c.has_more });
  });
});
