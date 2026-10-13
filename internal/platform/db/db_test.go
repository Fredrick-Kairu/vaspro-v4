package db_test

import (
	"context"
	"testing"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
)

func TestNewPool_InvalidDSN(t *testing.T) {
	_, err := vasdb.NewPool(context.Background(), "not a valid dsn \x00")
	if err == nil {
		t.Fatal("expected an error for a malformed DSN")
	}
}
