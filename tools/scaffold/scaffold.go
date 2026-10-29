// Command scaffold generates a new service from the template under
// templates/ (ROADMAP task 0.13). Run via `make new-service NAME=<svc>`.
package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"
)

//go:embed templates/*.tmpl
var templates embed.FS

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// reserved names a new service can't take: "platform" would collide
// with internal/platform, "all" and "vaspro_all"/"vaspro-all" would
// collide with cmd/vaspro-all.
var reserved = map[string]bool{
	"platform":   true,
	"all":        true,
	"vaspro_all": true,
}

// data is the template data every .tmpl file renders against.
type data struct {
	Name  string // lowercase, e.g. "demo" — package name, schema name, subject prefix
	Upper string // Name upper-cased — env var prefix, stream name
}

// file maps a template name to the path it renders to under root,
// relative to root/internal/<name>, root/cmd/<name> or
// root/migrations/<name> as appropriate.
type file struct {
	template string
	relPath  string // relative to root
}

// migrationFileName returns the goose timestamp-named filename for a
// new service's first migration (.claude/rules/migrations.md: "File
// names: goose timestamps with a short description").
func migrationFileName() string {
	return time.Now().UTC().Format("20060102150405") + "_init.sql"
}

func files(name string) []file {
	return []file{
		{"service.go.tmpl", filepath.Join("internal", name, name+".go")},
		{"domain.go.tmpl", filepath.Join("internal", name, "domain", "widget.go")},
		{"app.go.tmpl", filepath.Join("internal", name, "app", "widgets.go")},
		{"events.go.tmpl", filepath.Join("internal", name, "events", "events.go")},
		{"routes.go.tmpl", filepath.Join("internal", name, "api", "routes.go")},
		{"store_doc.go.tmpl", filepath.Join("internal", name, "store", "doc.go")},
		{"grpc_doc.go.tmpl", filepath.Join("internal", name, "grpc", "doc.go")},
		{"adapters_doc.go.tmpl", filepath.Join("internal", name, "adapters", "doc.go")},
		{"cmd_main.go.tmpl", filepath.Join("cmd", name, "main.go")},
		{"migration.sql.tmpl", filepath.Join("migrations", name, migrationFileName())},
	}
}

// ValidateName reports whether name is usable for a new service: lower-
// case letters, digits and underscores only, starting with a letter; not
// reserved; and its target directories don't already exist under root.
func ValidateName(root, name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("scaffold: %q is not a valid service name (want %s)", name, nameRE.String())
	}
	if reserved[name] {
		return fmt.Errorf("scaffold: %q is reserved", name)
	}
	for _, dir := range []string{
		filepath.Join(root, "internal", name),
		filepath.Join(root, "cmd", name),
		filepath.Join(root, "migrations", name),
	} {
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("scaffold: %s already exists", dir)
		}
	}
	return nil
}

// Generate writes a new service named name into root, following
// CLAUDE.md's service layout, and registers it in
// root/cmd/vaspro-all/main.go between that file's sentinel comments.
func Generate(root, name string) error {
	if err := ValidateName(root, name); err != nil {
		return err
	}

	d := data{Name: name, Upper: strings.ToUpper(name)}

	for _, f := range files(name) {
		if err := renderFile(root, f, d); err != nil {
			return err
		}
	}

	if err := registerInVasproAll(root, name); err != nil {
		return err
	}
	return nil
}

func renderFile(root string, f file, d data) error {
	tmpl, err := template.ParseFS(templates, "templates/"+f.template)
	if err != nil {
		return fmt.Errorf("scaffold: parsing template %s: %w", f.template, err)
	}

	outPath := filepath.Join(root, f.relPath)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o750); err != nil {
		return fmt.Errorf("scaffold: creating %s: %w", filepath.Dir(outPath), err)
	}

	out, err := os.Create(outPath) //nolint:gosec // outPath is built from a caller-supplied, already-validated service name under root
	if err != nil {
		return fmt.Errorf("scaffold: creating %s: %w", outPath, err)
	}
	defer out.Close() //nolint:errcheck // closed explicitly below; this is a backstop if Execute fails before that

	if err := tmpl.Execute(out, d); err != nil {
		return fmt.Errorf("scaffold: rendering %s: %w", f.template, err)
	}
	return out.Close()
}

const (
	importsBegin  = "// vaspro-all:imports-begin"
	servicesBegin = "// vaspro-all:services-begin"
)

// registerInVasproAll inserts an import for internal/<name> and a
// Bootstrap-and-register block into cmd/vaspro-all/main.go, right after
// that file's two sentinel comments — the one manual-looking step every
// `make new-service` run repeats mechanically.
func registerInVasproAll(root, name string) error {
	path := filepath.Join(root, "cmd", "vaspro-all", "main.go")
	content, err := os.ReadFile(path) //nolint:gosec // path is fixed, not user input
	if err != nil {
		return fmt.Errorf("scaffold: reading %s: %w", path, err)
	}

	importLine := fmt.Sprintf("\t\"github.com/Fredrick-Kairu/vaspro-v4/internal/%s\"", name)
	updated, err := insertAfter(string(content), importsBegin, importLine)
	if err != nil {
		return fmt.Errorf("scaffold: %w", err)
	}

	serviceBlock := fmt.Sprintf(`	if svc, err := %s.Bootstrap(ctx); err != nil {
		logger.Error("vaspro-all: %s: bootstrap", "error", err)
		os.Exit(1)
	} else {
		services = append(services, svc)
	}`, name, name)
	updated, err = insertAfter(updated, servicesBegin, serviceBlock)
	if err != nil {
		return fmt.Errorf("scaffold: %w", err)
	}

	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil { //nolint:gosec // path is built from a fixed suffix under root, not external input
		return fmt.Errorf("scaffold: writing %s: %w", path, err)
	}
	return nil
}

// insertAfter inserts line right after the first occurrence of the
// sentinel line in content.
func insertAfter(content, sentinel, line string) (string, error) {
	idx := strings.Index(content, sentinel)
	if idx < 0 {
		return "", fmt.Errorf("sentinel %q not found", sentinel)
	}
	lineEnd := strings.Index(content[idx:], "\n")
	if lineEnd < 0 {
		return "", fmt.Errorf("sentinel %q has no trailing newline", sentinel)
	}
	insertAt := idx + lineEnd + 1
	return content[:insertAt] + line + "\n" + content[insertAt:], nil
}
