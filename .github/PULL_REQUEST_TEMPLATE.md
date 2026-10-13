## What and why

<!-- What does this change do, and why? Link issues with "Fixes #123". -->

## How was it tested?

<!-- Commands you ran (make test, make lint, make sdk-test, make helm-lint, ...) and manual checks. -->

## Checklist

- [ ] Tests added or updated (`make test`; integration tests skip without `make services-up`)
- [ ] `make lint` passes (gofmt, go vet, golangci-lint)
- [ ] Public contract changes (URLs, site API, websocket, deploy gateway, SDK, config schema) are
      reflected in `docs/spec/SPEC.md` and, where relevant, in both editions (Go server and
      `deploy/cloudflare`) and the conformance suite
- [ ] SDK changes: `make sdk` was run and `web/sdk/backyard.{js,mjs}` are committed
- [ ] Docs, sample configs (`deploy/**/backyard.yaml`, Helm values) and the agent skill are updated
- [ ] No secrets, tokens or internal hostnames in code, tests or logs
