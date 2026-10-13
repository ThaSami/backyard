import type { Env as BackyardEnv } from "../src/env";

declare global {
  namespace Cloudflare {
    interface Env extends BackyardEnv {}
  }
}

export {};
