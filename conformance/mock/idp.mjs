#!/usr/bin/env node
// The mock OpenID Connect provider of the e2e harness (e2e/harness/mock-idp.mjs)
// as a process for conformance/run-go.sh's oidc pass, so the Claude connector's
// sign-in (SPEC §8.4) runs end to end without a browser or a real IdP. Its
// issuer is http://localhost:<port>; a request with the cookie
// idp_session=<user> (alice, bob, mallory) on that origin is signed in at once.
//
//   node mock/idp.mjs <port> <client id> <client secret> <redirect URI>...
//
// Prints "listening <port>" when ready; SIGINT/SIGTERM stop it.
import { startMockIdP } from "../../e2e/harness/mock-idp.mjs";

const [portArg, clientId, clientSecret, ...redirectURIs] = process.argv.slice(2);
const port = Number(portArg);
if (!Number.isInteger(port) || port <= 0 || !clientId || !clientSecret || redirectURIs.length === 0) {
  console.error("usage: node mock/idp.mjs <port> <client id> <client secret> <redirect URI>...");
  process.exit(2);
}

const idp = await startMockIdP({ port, clientId, clientSecret, redirectURIs, postLogoutRedirectURIs: [] });
console.log(`listening ${port} (issuer ${idp.issuer})`);
for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => {
    idp.close().then(() => process.exit(0));
  });
}
