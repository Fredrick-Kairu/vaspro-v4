// Command kycvendor runs the fake KYC vendor on its own, for the dev
// environment (docs/runbooks/dev-environment.md), and lets a tester
// decide its checks:
//
//	kycvendor serve
//	kycvendor list
//	kycvendor decide <check-id> passed|review|failed|expired
//
// serve reads KYCVENDOR_ADDR (default :8090), KYCVENDOR_WEBHOOK_SECRET
// (required; accounts' ACCOUNTS_KYC_WEBHOOK_SECRET), KYCVENDOR_WEBHOOK_URL
// (required; where results are posted) and KYCVENDOR_PUBLIC_URL (the base
// of hosted-flow links; defaults to http://localhost<addr>). list and
// decide talk to the server on KYCVENDOR_ADDR in the same container.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/kycvendor"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	addr := envOr("KYCVENDOR_ADDR", ":8090")
	var err error
	switch {
	case args[0] == "serve" && len(args) == 1:
		err = serve(addr)
	case args[0] == "list" && len(args) == 1:
		err = call(http.MethodGet, localURL(addr)+"/checks", "")
	case args[0] == "decide" && len(args) == 3:
		err = call(http.MethodPost, localURL(addr)+"/checks/"+args[1]+"/decision", fmt.Sprintf(`{"status":%q}`, args[2]))
	default:
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kycvendor:", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kycvendor serve | list | decide <check-id> passed|review|failed|expired")
}

func serve(addr string) error {
	secret := os.Getenv("KYCVENDOR_WEBHOOK_SECRET")
	webhookURL := os.Getenv("KYCVENDOR_WEBHOOK_URL")
	if secret == "" || webhookURL == "" {
		return errors.New("KYCVENDOR_WEBHOOK_SECRET and KYCVENDOR_WEBHOOK_URL are required")
	}
	vendor := kycvendor.NewStandalone([]byte(secret), envOr("KYCVENDOR_PUBLIC_URL", localURL(addr)), webhookURL)

	mux := http.NewServeMux()
	mux.Handle("/", vendor.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintln(os.Stderr, "kycvendor: listening on", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

func call(method, url, body string) error {
	// url is this container's own server (KYCVENDOR_ADDR), not request input.
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body)) //nolint:gosec // G704: see above
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	fmt.Print(string(out))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

// localURL is the server on addr as seen from inside its container.
func localURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	return "http://" + addr
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
