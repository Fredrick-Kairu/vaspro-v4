package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func loadRepoSpecs(t *testing.T) ([]loadedSpec, []byte) {
	t.Helper()
	var loaded []loadedSpec
	for _, s := range specs {
		data, err := os.ReadFile(filepath.Join("..", "..", s.Path))
		if err != nil {
			t.Fatal(err)
		}
		loaded = append(loaded, loadedSpec{specFile: s, Data: data})
	}
	catalogue, err := os.ReadFile(filepath.Join("..", "..", catalogueFile))
	if err != nil {
		t.Fatal(err)
	}
	return loaded, catalogue
}

// Every operation in the specs gets a section, and every name in
// extraCodes is a real operation and a real code.
func TestGenerate_CoversEveryOperation(t *testing.T) {
	loaded, catalogue := loadRepoSpecs(t)
	out, err := Generate(loaded, catalogue)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)

	ids := map[string]bool{}
	opID := regexp.MustCompile(`(?m)^\s+operationId: (\w+)$`)
	for _, s := range loaded {
		for _, m := range opID.FindAllStringSubmatch(string(s.Data), -1) {
			ids[m[1]] = true
			if !strings.Contains(doc, `<a id="`+anchor(m[1])+`"></a>`) {
				t.Errorf("no section for %s", m[1])
			}
		}
	}
	if len(ids) == 0 {
		t.Fatal("found no operations")
	}
	for id, codes := range extraCodes {
		if !ids[id] {
			t.Errorf("extraCodes names unknown operation %s", id)
		}
		for _, c := range codes {
			if !strings.Contains(string(catalogue), "code: "+c+"\n") {
				t.Errorf("extraCodes[%s] names unknown code %s", id, c)
			}
		}
	}
}

func TestCleanDescription(t *testing.T) {
	for in, want := range map[string]string{
		"Needs account:manage (task 2.8, OQ-09). Stored.":          "Needs account:manage. Stored.",
		"See system design §3 for this. The secret is shown once.": "The secret is shown once.",
		"Up to 10 MB (VAL-4008).":                                  "Up to 10 MB (VAL-4008).",
		"Folded\n  text   with spaces.":                            "Folded text with spaces.",
	} {
		if got := cleanDescription(in); got != want {
			t.Errorf("cleanDescription(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every spec in docs/api/ is in the reference: a new service's spec
// can't be left out (webhooks and contacts were, until 2026-11-18).
func TestSpecs_ListEverySpecInDocsAPI(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "api", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, s := range specs {
		listed[s.Path] = true
	}
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, ".oapi-codegen.yaml") {
			continue
		}
		n++
		if p := "docs/api/" + filepath.Base(f); !listed[p] {
			t.Errorf("%s isn't in specs", p)
		}
	}
	if n != len(specs) {
		t.Errorf("%d specs in docs/api, %d listed", n, len(specs))
	}
}

// OpenAPI 3.1 nullable types ([array, "null"]) render as their type:
// a nullable list's example is a list, and its items are described.
func TestGenerate_NullableTypes(t *testing.T) {
	spec := []byte(`openapi: 3.1.0
info: {title: T, version: "1"}
paths:
  /v1/things:
    post:
      operationId: createThing
      summary: Create a thing
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                codes:
                  type: [array, "null"]
                  items: {type: string}
                parts:
                  type: [array, "null"]
                  items:
                    type: object
                    properties:
                      label: {type: string}
                url: {type: [string, "null"]}
      responses:
        "200": {description: OK}
`)
	out, err := Generate([]loadedSpec{{specFile: specFile{Path: "t.yaml", Title: "T"}, Data: spec}}, []byte("codes: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	for _, want := range []string{
		"| `codes` | array of string \\| null |",
		"| `parts[].label` | string |",
		"| `url` | string \\| null |",
		`"codes": [`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("no %q in:\n%s", want, doc)
		}
	}
}
