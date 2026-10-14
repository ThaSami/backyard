/**
 * The authorization server of the remote MCP connector (SPEC §8.4): the
 * Worker issues its own OAuth 2.1 tokens to MCP clients such as Claude and
 * signs the browser in through its auth mode (Access or guests) at
 * /__backyard/auth/authorize.
 *
 * PINNED API (mcp-design.md §11, G4): the exports below are the contract with
 * src/mcp.ts and src/mcp-tools.ts. The bodies are placeholders until the
 * authorization server lands.
 */
import type { RequestContext } from "./router";
import type { User } from "./identity";

/** The authorize endpoint (GET, POST), served by serveAuth. */
export const AUTHORIZE_PATH = "/__backyard/auth/authorize";

/** One connection: the consent of a user to one MCP client (§4.3). */
export interface McpGrant {
  id: string;
  user_id: string;
  /** Snapshot taken at consent. */
  user: { id: string; email?: string; name: string; groups?: string[]; anonymous: boolean };
  client_id: string;
  client_name: string;
  client_kind: "metadata_document" | "registered";
  /** Host of a client metadata URL; "" for registered clients. */
  client_host: string;
  redirect_uri: string;
  scope: string;
  resource: string;
  /** Unix seconds. */
  created_at: number;
  code_exp: number;
  redeemed_at?: number | null;
  exp: number;
}

/** The fixed error_description values of an invalid access token (§3.10). */
export type McpTokenError =
  | "malformed token"
  | "token expired"
  | "token revoked"
  | "token not issued for this server"
  | "access removed by policy";

export type McpTokenResult = { ok: true; user: User; grant: McpGrant } | { ok: false; error: McpTokenError };

/**
 * Whether the connector is on for this request: MCP_ENABLED, a valid MCP
 * configuration and an https (or loopback) hub origin.
 */
export function mcpEnabled(ctx: RequestContext): boolean {
  void ctx;
  return false;
}

/** P: the canonical hub origin. */
export function mcpIssuer(ctx: RequestContext): string {
  void ctx;
  return "";
}

/** R: P + "/__backyard/mcp". */
export function mcpResource(ctx: RequestContext): string {
  void ctx;
  return "";
}

/** M: the protected resource metadata URL. */
export function mcpResourceMetadataURL(ctx: RequestContext): string {
  void ctx;
  return "";
}

/** Whether a present Origin header is accepted at /__backyard/mcp (§3.10 step 3). */
export function mcpAllowedOrigin(ctx: RequestContext, origin: string): boolean {
  void ctx;
  void origin;
  return false;
}

/**
 * Verifies an MCP access token and re-gates its user (§4.5). Throws on
 * infrastructure failures (answer 503).
 */
export async function verifyMcpAccessToken(ctx: RequestContext, raw: string): Promise<McpTokenResult> {
  void ctx;
  void raw;
  return { ok: false, error: "malformed token" };
}

/** The exact WWW-Authenticate value (§3.10); no error gives the missing-token form. */
export function mcpChallenge(ctx: RequestContext, error?: McpTokenError): string {
  void ctx;
  void error;
  return "";
}
