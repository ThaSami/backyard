---
description: Publish the current Backyard site (or a given folder) and return its URL
argument-hint: "[dir] [--site name]"
---

Publish a Backyard site. Arguments (optional): $ARGUMENTS

Follow the **backyard** skill (`references/deploy.md` covers the CLI in detail):

1. **Find the folder:** the directory given in the arguments; else the project's folder: the
   `"dir"` named in `.backyard.json` (in the current directory or a parent; `dir` is relative to
   that file, e.g. `dist` for build output, and defaults to the file's folder); else the current
   directory if it has an `index.html`. If there is nothing to deploy, say so and stop. A project
   with a build step publishes its output folder, never its sources: run the build first.
2. **Find the site name:** `--site` from the arguments; else `site` in `.backyard.json`; else
   the folder name. If there is no `.backyard.json` and the name is not obvious, ask the user.
3. **Sanity-check** what will be uploaded: `index.html` exists and loads
   `/__backyard/backyard.js` with that absolute path; the site's own assets use relative paths;
   no file that will be uploaded contains secrets (API keys, tokens, passwords). Dotfiles,
   `CLAUDE.md`, `AGENTS.md`, `.claude/`, `node_modules/` and `.backyardignore` patterns are
   never uploaded.
4. **Preview:** `backyard deploy <dir> --site <name> --dry-run` (or `backyard_deploy` with
   `dry_run: true`). A deploy replaces the whole site. If the site exists and was last deployed
   by someone else, or the dry run would delete files you did not expect, confirm with the user
   first.
5. **Deploy:** `backyard deploy <dir> --site <name>` (or `backyard_deploy`, which takes folders
   inside the current directory; pass any other folder, such as a parent, by its absolute path).
   Inside a project, `backyard deploy` without a folder (or `backyard_deploy` without `dir`)
   publishes the folder `.backyard.json` names.
   - "not logged in" / `401`: ask the user to run `backyard login --server <their Backyard URL>`
     in a terminal, then retry. No Backyard server yet? The user can create one for free on
     Cloudflare with `backyard cloudflare setup` (it logs the CLI in; it needs a release build of
     the CLI, since one from `go install` needs a checkout of the Backyard repository or
     `--package`), then retry.
   - `forbidden`: the site belongs to someone else; suggest a different name.
   - `too_large`: exclude big or unneeded files with `.backyardignore`, or deploy the build
     output folder.
6. **Report** the URL and what changed (uploaded, unchanged and deleted files).
