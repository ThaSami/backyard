// Package docs holds Backyard's user and operator documentation (Markdown,
// including the normative design specification in spec/SPEC.md). It has no
// code; its tests keep the documentation honest (CLAUDE.md, invariant 9):
//
//   - relative links in docs/ and the repository's top-level guides resolve
//     to files that exist, and their #anchors to headings that exist;
//   - every `make <target>` the guides mention is a target of the Makefile;
//   - every key in configuration.md exists in the configuration schema
//     (internal/config), and every key of the schema has a row there;
//   - every "SPEC §n" reference names a section the specification has;
//   - statements tied to code match it (contract_test.go): the key derivation
//     labels of both editions, the site URL examples of SPEC §2, the limits
//     where a negative value means unlimited, the quoted oidc host-mode
//     configuration errors, the AI request limits, and the MCP tools and
//     their parameters;
//   - and so do the statements about hardened behaviour
//     (contract_hardening_test.go): the passive upload types of both
//     editions (SPEC §3), the warehouse result cap, the realtime payload
//     limit, the flags of the direct-sync commands, the files no ignore
//     configuration can deploy, the login state cookie, and the Cloudflare
//     edition's query page budget and Access bypass paths.
package docs
