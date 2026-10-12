// Package service implements warehouse.Service on top of configured sources.
package service

import (
	"context"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
)

// New connects to every configured source lazily (on first query) and
// returns the policy-enforcing service. With no sources, Enabled() is false
// and Query returns warehouse.ErrDisabled.
// STUB: replaced by the warehouse implementation.
func New(ctx context.Context, cfg config.WarehouseConfig) (warehouse.Service, error) {
	panic("warehouse/service: not implemented")
}
