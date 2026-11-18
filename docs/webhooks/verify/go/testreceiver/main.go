// Command testreceiver is a webhook endpoint that checks every request
// with vasprowebhook.Verify, as a client's would. VASPRO's Phase 5
// end-to-end test points an endpoint at it; it also shows a whole
// handler: the raw body verified before anything reads it, the
// verification challenge echoed, and deliveries deduplicated by
// Vaspro-Webhook-Id.
//
//	testreceiver -addr 127.0.0.1:9900 -secret whsec_… [-fail-first wallet.credited]
//
// POST /hook takes webhooks. GET /received lists what was verified (the
// test reads it). -fail-first answers the first delivery of that event
// type with 500, so VASPRO retries it.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/docs/webhooks/verify/go/vasprowebhook"
)

// received is one verified request.
type received struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type receiver struct {
	secret    string
	failFirst string

	mu       sync.Mutex
	verified []received
	rejected int
	seen     map[string]bool // event IDs handled, for deduplication
	failed   bool
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9900", "where to listen")
	secret := flag.String("secret", "", "the endpoint's secret, whsec_… (required)")
	failFirst := flag.String("fail-first", "", "answer the first delivery of this event type with 500")
	flag.Parse()
	if *secret == "" {
		log.Fatal("testreceiver: -secret is required")
	}
	rcv := &receiver{secret: *secret, failFirst: *failFirst, seen: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", rcv.hook)
	mux.HandleFunc("GET /received", rcv.list)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func (rcv *receiver) hook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	// Verify the raw bytes before decoding them.
	if err := vasprowebhook.Verify(rcv.secret,
		r.Header.Get("Vaspro-Webhook-Timestamp"),
		r.Header.Get("Vaspro-Webhook-Signature"),
		body, time.Now()); err != nil {
		rcv.mu.Lock()
		rcv.rejected++
		rcv.mu.Unlock()
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	var event struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Challenge string `json:"challenge"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "not an event", http.StatusBadRequest)
		return
	}
	if event.Type == "webhook.verification" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": event.Data.Challenge})
		return
	}

	rcv.mu.Lock()
	defer rcv.mu.Unlock()
	if event.Type == rcv.failFirst && !rcv.failed {
		rcv.failed = true
		http.Error(w, "failing once on purpose", http.StatusInternalServerError)
		return
	}
	// The same event may arrive more than once; handle it once.
	if !rcv.seen[event.ID] {
		rcv.seen[event.ID] = true
		rcv.verified = append(rcv.verified, received{ID: event.ID, Type: event.Type})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (rcv *receiver) list(w http.ResponseWriter, _ *http.Request) {
	rcv.mu.Lock()
	defer rcv.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"verified": rcv.verified, "rejected": rcv.rejected})
}
