// Command apidocs generates docs/api/API.md, the API reference handed to
// testers and the frontend team, from the OpenAPI specs in docs/api/ and
// the error catalogue. The hand-written introduction is intro.md in this
// directory. Run via `make gen`; `make gen-check` fails if API.md is stale.
package main

import (
	"fmt"
	"os"
)

const outputFile = "docs/api/API.md"

var specs = []specFile{
	{Path: "docs/api/identity.yaml", Title: "Identity API"},
	{Path: "docs/api/accounts.yaml", Title: "Accounts API"},
	{Path: "docs/api/ledger.yaml", Title: "Ledger API"},
	{Path: "docs/api/payments.yaml", Title: "Payments API"},
}

const catalogueFile = "docs/errors/catalogue.yaml"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	loaded := make([]loadedSpec, 0, len(specs))
	for _, s := range specs {
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return fmt.Errorf("apidocs: reading %s: %w", s.Path, err)
		}
		loaded = append(loaded, loadedSpec{specFile: s, Data: data})
	}
	catalogue, err := os.ReadFile(catalogueFile)
	if err != nil {
		return fmt.Errorf("apidocs: reading %s: %w", catalogueFile, err)
	}

	out, err := Generate(loaded, catalogue)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outputFile, out, 0o644); err != nil { //nolint:gosec // generated documentation, not a secret; 0644 matches every other generated file in the repo
		return fmt.Errorf("apidocs: writing %s: %w", outputFile, err)
	}
	return nil
}
