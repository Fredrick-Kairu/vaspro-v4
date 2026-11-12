//go:build contract

package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const eventsDir = "../../docs/events"

// webhookKeyword marks whether an event is sent to the client it belongs
// to as a webhook (OQ-23, data model §8).
const webhookKeyword = "x-vaspro-webhook"

// TestEventCatalogue_EverySchemaSaysWhetherClientsGetIt keeps the
// webhook allowlist explicit: a new event schema can't land without
// deciding whether clients see it, and one sent to clients must name
// the client.
func TestEventCatalogue_EverySchemaSaysWhetherClientsGetIt(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(eventsDir, "*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no schemas found in %s", eventsDir)
	}

	clients := 0
	for _, file := range files {
		eventType := strings.TrimSuffix(filepath.Base(file), ".schema.json")
		t.Run(eventType, func(t *testing.T) {
			raw, err := os.ReadFile(file) //nolint:gosec // a path from the repository's own catalogue
			if err != nil {
				t.Fatal(err)
			}

			var schema struct {
				Title    string           `json:"title"`
				Type     string           `json:"type"`
				Required []string         `json:"required"`
				Props    map[string]any   `json:"properties"`
				Webhook  *json.RawMessage `json:"x-vaspro-webhook"`
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			if schema.Title != eventType {
				t.Errorf("title = %q, want the file's event type %q", schema.Title, eventType)
			}
			if schema.Webhook == nil {
				t.Fatalf("%s is missing; say whether clients receive this event", webhookKeyword)
			}
			var sent bool
			if err := json.Unmarshal(*schema.Webhook, &sent); err != nil {
				t.Fatalf("%s = %s, want true or false", webhookKeyword, *schema.Webhook)
			}
			if sent {
				clients++
				if _, ok := schema.Props["tenant_id"]; !ok || !slices.Contains(schema.Required, "tenant_id") {
					t.Errorf("sent to clients but tenant_id isn't a required property, so there's no client to send it to")
				}
			}

			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			comp := jsonschema.NewCompiler()
			if err := comp.AddResource(filepath.Base(file), doc); err != nil {
				t.Fatal(err)
			}
			if _, err := comp.Compile(filepath.Base(file)); err != nil {
				t.Errorf("doesn't compile: %v", err)
			}
		})
	}
	if clients == 0 {
		t.Error("no event is sent to clients")
	}
}
