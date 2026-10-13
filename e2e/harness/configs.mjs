// Server configurations used by the e2e harness (written as JSON, which is valid YAML).
import { createHash, randomBytes } from "node:crypto";
import { join } from "node:path";

import { FAKE_AI_KEY } from "./fake-ai.mjs";

export const sha256Hex = (s) => createHash("sha256").update(s).digest("hex");

/** A fresh server.secret. */
export const newSecret = () => randomBytes(32).toString("hex");

/** Rate limits high enough that parallel guests (keyed by client IP) never collide. */
export const RELAXED_LIMITS = {
  api_per_minute: 100000,
  db_writes_per_minute: 100000,
  ai_per_minute: 10000,
  uploads_per_minute: 10000,
  deploys_per_hour: 10000,
  warehouse_per_minute: 10000,
};

/**
 * The config of a `backyard serve --dev` server on 127.0.0.1:port: host mode
 * on *.localhost, guests (auth none) plus an admin static token, fs storage
 * and sqlite under dir, and the fake AI upstream when given. A fixed secret
 * keeps guest identities valid across restarts.
 */
export function devServerConfig({ dir, port, name, adminToken, fakeAI, secret = newSecret() }) {
  const config = {
    server: { listen: `127.0.0.1:${port}`, dev: true, name, secret },
    auth: { mode: "none", tokens: [{ name: "e2e-admin", sha256: sha256Hex(adminToken), admin: true }] },
    storage: { type: "fs", fs: { root: join(dir, "data") } },
    database: { type: "sqlite", sqlite: { path: join(dir, "backyard.db") } },
    ratelimit: RELAXED_LIMITS,
    realtime: { max_conns_per_user: 1000 },
    telemetry: { log_level: "warn", log_format: "text" },
  };
  if (fakeAI) {
    config.ai = {
      default_model: "smart",
      providers: { fake: { type: "openai", base_url: fakeAI.url, api_key: FAKE_AI_KEY } },
      models: {
        smart: { provider: "fake", model: "fake-chat", kind: "chat", description: "Fake chat model" },
        quick: { provider: "fake", model: "fake-chat-mini", kind: "chat", description: "Smaller fake model" },
        image: { provider: "fake", model: "fake-image", kind: "image" },
        embed: { provider: "fake", model: "fake-embed", kind: "embedding" },
      },
    };
  }
  return config;
}

/**
 * The config of a server in auth.mode oidc (path mode on 127.0.0.1:port)
 * against the mock IdP: admins by group, only example.com addresses, claims
 * enriched from userinfo, and a static deploy token.
 */
export function oidcServerConfig({ dir, port, publicURL, issuer, clientId, clientSecret, deployToken }) {
  return {
    server: {
      listen: `127.0.0.1:${port}`,
      base_domain: "127.0.0.1",
      public_url: publicURL,
      url_mode: "path",
      name: "Backyard SSO",
      secret: newSecret(),
    },
    auth: {
      mode: "oidc",
      admin_groups: ["backyard-admins"],
      allowed_domains: ["example.com"],
      tokens: [{ name: "e2e-deploy", sha256: sha256Hex(deployToken) }],
      oidc: {
        issuer,
        client_id: clientId,
        client_secret: clientSecret,
        scopes: ["openid", "profile", "email", "groups"],
        use_userinfo: true,
      },
    },
    identity: { extra_claims: ["location"] },
    storage: { type: "fs", fs: { root: join(dir, "data") } },
    database: { type: "sqlite", sqlite: { path: join(dir, "backyard.db") } },
    ratelimit: RELAXED_LIMITS,
    telemetry: { log_level: "warn", log_format: "text" },
  };
}
