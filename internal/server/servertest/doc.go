// Package servertest holds end-to-end tests that run the HTTP server against
// real components: the filesystem blob store, the SQLite (and, when
// BACKYARD_TEST_POSTGRES_DSN is set, PostgreSQL) document store, the realtime
// hub over in-memory (and, when BACKYARD_TEST_REDIS_URL is set, Redis)
// pub/sub, the auth package in mode "none" and the embedded deploy gateway.
//
// It contains no production code; keeping these tests in their own package
// isolates the server unit tests from the backend packages' dependencies.
package servertest
