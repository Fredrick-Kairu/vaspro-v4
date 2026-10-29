package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
)

const defaultSchemaPath = "packs/schema.json"

// packsValidate validates each pack in packPaths against schemaPath,
// writing one pass/fail line per pack to out. Returns true only if
// every pack validated cleanly.
func packsValidate(out io.Writer, schemaPath string, packPaths []string) bool {
	allOK := true
	for _, p := range packPaths {
		if _, err := packs.Load(schemaPath, p); err != nil {
			_, _ = fmt.Fprintf(out, "FAIL %s: %v\n", p, err)
			allOK = false
			continue
		}
		_, _ = fmt.Fprintf(out, "ok   %s\n", p)
	}
	return allOK
}

// discoverPacks finds every packs/<cc>/pack.yaml under root.
func discoverPacks(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, "packs", "*", "pack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("vaspro: discovering packs under %s: %w", root, err)
	}
	return matches, nil
}
