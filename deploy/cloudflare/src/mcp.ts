/**
 * The remote MCP endpoint POST /__backyard/mcp (SPEC §14.1): Streamable HTTP,
 * stateless, JSON responses only, authenticated by MCP access tokens of the
 * connector's authorization server (src/oauth.ts).
 *
 * PINNED API (mcp-design.md §11, G4): hub.ts routes /__backyard/mcp here.
 * The body is a placeholder until the endpoint lands.
 */
import { errorResponse, notFound } from "./http";
import type { RequestContext } from "./router";

/** Serves /__backyard/mcp on the canonical hub host (any method; §3.10). */
export async function serveMcp(ctx: RequestContext): Promise<Response> {
  void ctx;
  return errorResponse(notFound("the Claude connector is not enabled on this server"));
}
