// Package identitytest holds integration tests that run the identity user
// cache against the real docstore backends (memory, SQLite and, when the
// BACKYARD_TEST_* services are available, PostgreSQL and DynamoDB Local). It
// contains no production code; keeping the tests in their own package isolates
// the identity unit tests from the backend packages' dependencies.
package identitytest
