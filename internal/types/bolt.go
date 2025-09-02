package types

import (
	"context"
	"time"
)

type PersistenceConfig struct {
	Driver          string
	DSN             string
	FlushIntervalMs time.Duration
}

type Window struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
}

type KeyState struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Enabled       bool      `json:"enabled"`
	Weight        int       `json:"weight"`
	DisabledUntil time.Time `json:"disabled_until"`
	LatEwmaMs     float64   `json:"lat_ewma_ms"`

	MinReq Window `json:"min_req"`
	MinTok Window `json:"min_tok"`
	DayReq Window `json:"day_req"`
	DayTok Window `json:"day_tok"`

	UpdatedAt time.Time `json:"updated_at"`
}

// type Store interface {
// 	LoadAll(ctx context.Context) ([]*KeyState, error)
// 	Upsert(ctx context.Context, ks *KeyState) error
// 	Close() error
// }

type Store interface {
	// Load all known readiness timestamps at startup.
	LoadAllReady(ctx context.Context) (map[string]time.Time, error)

	// Enqueue an update; must NOT block the caller.
	UpsertReadyAtAsync(keyID string, nextReady time.Time)

	// Close stops the background worker and flushes pending updates.
	Close() error
}
