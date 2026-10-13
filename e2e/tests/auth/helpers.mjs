// Helpers for the tests against the OIDC server.

/**
 * A new browser context (a person). With idpUser ("alice", "bob", …) it
 * already has a session at the mock IdP, so signing in to Backyard is silent.
 */
export async function person(newUser, state, idpUser) {
  const ctx = await newUser();
  if (idpUser) await ctx.addCookies([{ name: "idp_session", value: idpUser, url: state.idp.issuer, httpOnly: true, sameSite: "Lax" }]);
  return ctx;
}

/** Opens url as a signed-in idpUser and waits for the SDK. */
export async function signedInPage(newUser, state, idpUser, url) {
  const page = await (await person(newUser, state, idpUser)).newPage();
  await page.goto(url);
  await page.waitForURL(url);
  await page.waitForFunction(() => typeof window.backyard === "object" && window.backyard !== null);
  return page;
}

/** The mock IdP's event log. */
export const idpLog = async (state) => (await fetch(`${state.idp.issuer}/__log`)).json();
