package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
)

type testConfig struct {
	Port int    `env:"PORT,required"`
	Name string `env:"NAME" envDefault:"vaspro"`
}

func (c testConfig) Validate() error {
	if c.Port < 0 {
		return errors.New("PORT must not be negative")
	}
	return nil
}

func TestLoad_RequiredFieldMissing(t *testing.T) {
	_, err := config.Load[testConfig]()
	if err == nil {
		t.Fatal("expected an error when PORT is unset, got nil")
	}
	if !strings.Contains(err.Error(), "PORT") {
		t.Fatalf("expected the error to name PORT, got: %v", err)
	}
}

func TestLoad_DefaultApplied(t *testing.T) {
	t.Setenv("PORT", "8080")

	cfg, err := config.Load[testConfig]()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Name != "vaspro" {
		t.Fatalf("expected default Name %q, got %q", "vaspro", cfg.Name)
	}
	if cfg.Port != 8080 {
		t.Fatalf("expected Port 8080, got %d", cfg.Port)
	}
}

func TestLoad_ValidateErrorSurfaces(t *testing.T) {
	t.Setenv("PORT", "-1")

	_, err := config.Load[testConfig]()
	if err == nil {
		t.Fatal("expected Validate's error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("expected the Validate error text, got: %v", err)
	}
}

func TestLoad_ValidRoundTrip(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("NAME", "identity")

	cfg, err := config.Load[testConfig]()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9090 || cfg.Name != "identity" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestMustLoad_PanicsOnInvalidInput(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected MustLoad to panic when PORT is unset")
		}
	}()
	config.MustLoad[testConfig]()
}

func TestMustLoad_ReturnsOnValidInput(t *testing.T) {
	t.Setenv("PORT", "1234")

	cfg := config.MustLoad[testConfig]()
	if cfg.Port != 1234 {
		t.Fatalf("expected Port 1234, got %d", cfg.Port)
	}
}
