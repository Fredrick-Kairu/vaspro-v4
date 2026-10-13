// Package config loads process configuration from environment variables
// and validates it at start-up, before anything else runs.
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

type validator interface {
	Validate() error
}

// Load parses environment variables into a T using its `env:"..."` struct
// tags, then calls T.Validate, if it implements one.
func Load[T any]() (T, error) {
	var cfg T
	if err := env.Parse(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	if v, ok := any(cfg).(validator); ok {
		if err := v.Validate(); err != nil {
			return cfg, fmt.Errorf("config: %w", err)
		}
	}
	return cfg, nil
}

// MustLoad is Load, panicking on error. Only call it once, at process
// start-up — never in a request path.
func MustLoad[T any]() T {
	cfg, err := Load[T]()
	if err != nil {
		panic(err)
	}
	return cfg
}
