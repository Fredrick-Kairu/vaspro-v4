//go:build toolsdeps

// This file is never actually built — "toolsdeps" is never passed to
// `go build`/`go vet`/`golangci-lint` anywhere in this repo. Its only
// job is to keep go-chi/chi/v5 in go.mod: the real import lives in
// templates/routes.go.tmpl, a text/template file go mod tidy can't see,
// so without this, tidy drops chi as soon as no generated service
// happens to exist in the tree (exactly what `make new-service` needs
// on the very next run). golang.org/x/sync/errgroup doesn't need this
// treatment — cmd/vaspro-all/main.go imports it directly and
// unconditionally, with or without any service registered.
package toolsdeps

import _ "github.com/go-chi/chi/v5"
