// Command errcodes generates internal/platform/problem/codes_gen.go from
// docs/errors/catalogue.yaml. Run via `make gen`.
package main

import (
	"fmt"
	"os"
)

const (
	catalogueFile = "docs/errors/catalogue.yaml"
	outputFile    = "internal/platform/problem/codes_gen.go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	data, err := os.ReadFile(catalogueFile)
	if err != nil {
		return fmt.Errorf("errcodes: reading %s: %w", catalogueFile, err)
	}

	out, err := Generate(data)
	if err != nil {
		return err
	}

	if err := os.WriteFile(outputFile, out, 0o644); err != nil { //nolint:gosec // generated Go source, not a secret; 0644 matches every other generated file in the repo
		return fmt.Errorf("errcodes: writing %s: %w", outputFile, err)
	}
	return nil
}
