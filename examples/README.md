# Backyard examples

Six complete Backyard sites. Each folder is deployable as-is: plain HTML, CSS and JavaScript,
no build step, no external CDNs, light and dark mode. They use only the documented client API
(`/__backyard/backyard.js`, see `skills/backyard/references/api.md`), so they run unchanged on
the Go server and on the Cloudflare edition. Each also ships a `preview.svg`: a hand-drawn,
self-contained 1280×720 sketch of its main screen that the hub shows on the site's card (see
[Site preview](../docs/deploying-sites.md#site-preview)).

| Example | What it shows | APIs |
|---|---|---|
| [`lunch-vote`](lunch-vote/) | Add lunch spots and vote once per day; results re-sort live, avatars show who is deciding right now. | `db` + `watch`, rooms (presence), `me()` |
| [`live-poll`](live-poll/) | All-hands poll: the host (`?host`) starts rounds, the audience answers on their phones, animated result bars update live. | `db` + `watch` (re-targeted per round), rooms |
| [`cursors`](cursors/) | Multiplayer cursors with names, click reactions and Figma-style cursor chat (press `/`), throttled to ~20 updates/s. | rooms: presence state, `send`, join/leave events |
| [`ai-chat`](ai-chat/) | Streaming chat with a model picker (from `ai.models()`), answer styles, safe Markdown rendering. | `ai.models`, `ai.stream` |
| [`file-drop`](file-drop/) | Drag-and-drop/paste uploads, live gallery with filters, search, image viewer and delete. | `files.*`, `db.subscribe` as a change signal, `id.lookup` |
| [`kpi-dashboard`](kpi-dashboard/) | Stat tiles, an SVG trend chart with crosshair tooltip and a data table from a warehouse **named query**; labeled demo data when the warehouse is unavailable. | `warehouse.sources`, `warehouse.named` |

## Try one locally

```sh
backyard dev examples/lunch-vote          # → http://lunch-vote.localhost:4321/
```

`backyard dev` serves the folder with a complete local backend (database, realtime, files) and
anonymous guest identities. Open the page in a normal and a private browser window (each gets
its own guest identity) to see the realtime features. AI and the warehouse need providers:
start the preview with a server config, e.g.
`backyard dev examples/ai-chat --config backyard.yaml`. Without them `ai-chat` explains that AI
is not enabled and `kpi-dashboard` shows demo data.

## Deploy one

```sh
backyard deploy examples/lunch-vote --site lunch-vote
```

Pick a site name that is not taken (`backyard sites info <name>` says "not found"); deploying
replaces whatever is live under that name. No Backyard server yet? `backyard cloudflare setup`
creates one on your own Cloudflare account (free plan; Node.js 22 or later, no Terraform; no
checkout with a release build of the CLI, while one from `go install` needs a checkout or
`--package`, see [Cloudflare](../docs/cloudflare.md#set-it-up)) and logs the CLI in; the same
`backyard deploy` command then publishes to
`https://backyard.<account>.workers.dev/s/lunch-vote/`. To customize an example, copy the
folder, run `backyard init <name>` inside it to add `.backyard.json` and the agent docs, and
edit away.

## Notes

- **live-poll:** open `https://<site>/?host` on the presenter's screen and share the plain URL
  (the host view shows it with a copy button). Answers are one per person per round; the
  audience sees results after answering or when voting closes.
- **cursors:** positions are fractions of the window so different screen sizes line up; nothing
  is stored, everything travels through one room.
- **ai-chat:** lists the chat models (aliases) your admins configured; the conversation stays in
  the browser tab (`sessionStorage`). No API key ever reaches the browser.
- **file-drop:** files have no realtime feed of their own, so every upload or delete also writes
  one document (`signals/files`) that all open tabs subscribe to, then reload the list.
- **kpi-dashboard:** runs the named query `kpi_daily` (or `?query=<name>`). It expects one row
  per day: a date column followed by numeric columns. Define it in the server config:

  ```yaml
  warehouse:
    sources:
      default:
        type: postgres            # or bigquery / athena
        dsn: ${ANALYTICS_DSN}
    queries:
      kpi_daily:
        description: Orders and returns per day
        params: [days]
        sql: |
          SELECT day, orders, returns
          FROM analytics.daily_orders
          WHERE day >= CURRENT_DATE - CAST(@days AS integer)
          ORDER BY day
  ```

  The warehouse is not available on the Cloudflare edition; the dashboard then shows demo data.

More patterns (chat room, kanban, guestbook, image generator, game leaderboard…) are in
[`skills/backyard/references/recipes.md`](../skills/backyard/references/recipes.md).
