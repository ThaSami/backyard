---
description: Build and publish a new Backyard site from a one-line idea (scaffold, build, check, deploy)
argument-hint: <idea, e.g. "realtime lunch-spot voting for my team">
---

Build a new internal web app on Backyard from this idea and publish it:

$ARGUMENTS

Use the **backyard** skill (load it now if it is not loaded yet) for the client API, golden
rules and patterns; its `references/recipes.md` has complete apps to start from and
`references/api.md` has every exact signature. Then:

1. **Understand.** If no idea was given, or it is too vague to build, ask one short question.
   Otherwise restate the app in one line and choose sensible defaults instead of asking more.
2. **Name.** Pick a short, descriptive site name: lowercase letters, digits and `-`, at most 63
   characters (e.g. `lunch-vote`). Check that it is free with `backyard sites info <name>`
   ("not found" means free) or the `backyard_list_sites` MCP tool. If an unrelated site already
   uses it, pick another name; never overwrite someone else's site.
3. **Scaffold.** Run `backyard init <name>` in the current directory (or the `backyard_init`
   tool with `dir` set to a new folder) and work inside the new folder. Keep `.backyard.json`;
   replace the starter `index.html`.
4. **Build.** Static files only (`index.html`, plus `app.js` / `style.css` when useful):
   - load `<script src="/__backyard/backyard.js"></script>` with this absolute path before your
     code, and your own files with relative paths;
   - shared data in `backyard.db`, rendered with `collection.watch(...)` so it is live; presence
     and cursors with `backyard.socket.room(...)`; AI with `backyard.ai`; uploads with
     `backyard.files`; metrics with `backyard.warehouse` plus a graceful fallback when it answers
     `unsupported`;
   - identity from `await backyard.me()` (never a login form or a name field);
   - a polished, responsive UI with loading, empty and error states; user content inserted with
     `textContent`; no secrets and no external CDNs.
5. **Check.** Re-read the code against `references/api.md` (method names, argument shapes,
   query operators, document id rules). When possible, start `backyard dev --no-open` in the
   background, check that it serves the site with
   `curl -s -H 'Host: <name>.localhost:4321' http://127.0.0.1:4321/` (the response must be your
   page, not the dev hub), then stop it. Use curl: outside browsers, `*.localhost` names often
   do not resolve (Node `fetch`, wget and Python fail with "not found" in containers and CI),
   and `http://localhost:4321/` is the dev hub, not the site.
6. **Deploy.** Run `backyard deploy` in the site folder (or the `backyard_deploy` tool). If it
   reports that you are not logged in, stop and ask the user to run
   `backyard login --server <their Backyard URL>` in a terminal, then retry. If the user has no
   Backyard server at all, offer the free option: they run `backyard cloudflare setup` in their
   own terminal (their own Cloudflare account on the free plan; needs Node.js 22+, a browser
   login and a release build of the CLI, since one from `go install` needs a checkout of the
   Backyard repository or `--package`), which also logs the CLI in; then deploy. Never ask
   them to paste Cloudflare tokens into the chat. Sites there are path-mode URLs, open to
   anyone with the link by default, with no warehouse.
7. **Report.** Reply with the live URL, a two-line summary of what the app does and where its
   data lives, and how to change it (edit the files, then `/backyard:deploy`).
