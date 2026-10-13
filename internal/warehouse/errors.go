package warehouse

import "errors"

// Additional sentinel errors. Together with ErrDisabled, ErrForbidden,
// ErrNotFound and ErrBadRequest they let the HTTP layer map every failure to a
// SPEC §5 error code without inspecting engine-specific error types:
//
//	ErrDisabled   → 501 unsupported (or 404 not_found)
//	ErrBadRequest → 400 bad_request   (invalid params, SQL syntax / semantic errors)
//	ErrForbidden  → 403 forbidden     (raw SQL disabled, statement guard, read-only violation, permission denied)
//	ErrNotFound   → 404 not_found     (unknown source or named query)
//	ErrTimeout    → 504 upstream_error (warehouse.timeout exceeded)
//	ErrUpstream   → 502 upstream_error (engine or connectivity failure)
//
// Errors returned by Service.Query wrap exactly one of these sentinels, except
// when the caller's own context was canceled (then they wrap context.Canceled).
var (
	// ErrUpstream: the engine failed to run the query (connection failure,
	// internal engine error, quota exhaustion, …).
	ErrUpstream = errors.New("warehouse: upstream error")
	// ErrTimeout: the query exceeded warehouse.timeout (or the engine's
	// statement timeout).
	ErrTimeout = errors.New("warehouse: query timed out")
)
