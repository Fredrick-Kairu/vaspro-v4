//go:build integration

package contacts_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	contactsv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/contacts/v1"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/contactaccess"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

// config is the environment's defaults, with test keys and addresses.
func config() contacts.Config {
	return contacts.Config{
		PacksDir: "../../packs", ObjectStorageEndpoint: "127.0.0.1:1", ObjectStoragePublicEndpoint: "127.0.0.1:1",
		ObjectStorageAccessKey: "k", ObjectStorageSecretKey: "s", ObjectStorageBucket: "contacts", ObjectStorageRegion: "us-east-1",
		ClamAVAddr: "127.0.0.1:1", FilesKMSKey: key(1), DownloadKey: key(2), DownloadBaseURL: "https://api.vaspro.test",
		MaxUploadBytes: 20 << 20, FileRetention: 48 * time.Hour, WorkerPoll: 2 * time.Second, SweepInterval: time.Minute,
	}
}

func TestNew_RefusesSettingsThatCantWork(t *testing.T) {
	for name, mutate := range map[string]func(*contacts.Config){
		"a files key that isn't 32 bytes":  func(c *contacts.Config) { c.FilesKMSKey = base64.StdEncoding.EncodeToString([]byte("short")) },
		"a download key that isn't base64": func(c *contacts.Config) { c.DownloadKey = "not base64!" },
		"no worker interval":               func(c *contacts.Config) { c.WorkerPoll = 0 },
		"no sweep interval":                func(c *contacts.Config) { c.SweepInterval = 0 },
		"a packs directory with no packs":  func(c *contacts.Config) { c.PacksDir = t.TempDir() },
		"no upload size":                   func(c *contacts.Config) { c.MaxUploadBytes = 0 },
		"no file retention":                func(c *contacts.Config) { c.FileRetention = 0 },
	} {
		cfg := config()
		mutate(&cfg)
		if _, err := contacts.New(cfg, nil, nil, contacts.Clients{}); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}

// TestService_ImportsEndToEnd runs the service as production wires it
// (a role granted contacts_service): /readyz; an import created over the
// HTTP API, uploaded through its signed form and confirmed; then the
// service's own worker loop processes it, and contact_import.finished
// reaches NATS. It shuts down cleanly.
func TestService_ImportsEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../migrations/contacts"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE contacts_app LOGIN PASSWORD 'contacts_app'; GRANT contacts_service TO contacts_app;`); err != nil {
		t.Fatal(err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("contacts_app", "contacts_app")+"&search_path=contacts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	b, err := bus.Connect(ctx, testkit.StartNATSURL(t))
	if err != nil {
		t.Fatal(err)
	}
	storage := testkit.StartObjectStorage(t)
	scanner, err := clamd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scanner.Close)

	accounts := contactaccess.New()
	cfg := config()
	cfg.HTTPAddr, cfg.GRPCAddr, cfg.WorkerPoll = freeAddr(t), freeAddr(t), 100*time.Millisecond
	cfg.ObjectStorageEndpoint, cfg.ObjectStoragePublicEndpoint = storage.Endpoint, storage.Endpoint
	cfg.ObjectStorageAccessKey, cfg.ObjectStorageSecretKey = storage.AccessKey, storage.SecretKey
	cfg.ClamAVAddr = scanner.Addr()
	svc, err := contacts.New(cfg, pool, b, contacts.Clients{Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://" + cfg.HTTPAddr + "/readyz") //nolint:gosec,noctx // the test's own server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never became ready: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var (
		mu       sync.Mutex
		finished []events.Envelope
	)
	consumer, err := b.Consume(ctx, bus.ConsumeConfig{Stream: "CONTACTS_EVENTS", Durable: "test", FilterSubject: "contacts.contact_import.finished", AckWait: 5 * time.Second, MaxDeliver: 1},
		func(_ context.Context, raw []byte) error {
			var env events.Envelope
			_ = json.Unmarshal(raw, &env)
			mu.Lock()
			finished = append(finished, env)
			mu.Unlock()
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Stop()

	user, tenant := uuid.New(), uuid.New()
	accounts.Grant(user, tenant, app.PermRead, app.PermManage)
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, "http://"+cfg.HTTPAddr+"/v1/contacts/tenants/"+tenant.String()+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req.Header.Set(api.HeaderUserID, user.String())
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, out)
		}
		return out
	}
	created := call(http.MethodPost, "/imports", map[string]any{"file_name": "list.csv", "content_type": "text/csv"})
	id := created["import"].(map[string]any)["id"].(string)
	upload := created["upload"].(map[string]any)
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	for k, v := range upload["fields"].(map[string]any) {
		_ = mw.WriteField(k, v.(string))
	}
	fw, _ := mw.CreateFormFile("file", "list.csv")
	_, _ = io.WriteString(fw, "phone,first_name\n0700000001,Ann\n0700000002,Ben\n")
	_ = mw.Close()
	resp, err := http.Post(upload["url"].(string), mw.FormDataContentType(), &form) //nolint:gosec,noctx // the test's storage
	if err != nil || resp.StatusCode/100 != 2 {
		t.Fatalf("upload: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	call(http.MethodPost, "/imports/"+id+"/upload-confirmation", nil)

	deadline = time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		n := len(finished)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("contact_import.finished never reached NATS; the import is %v", call(http.MethodGet, "/imports/"+id, nil))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := call(http.MethodGet, "/imports/"+id, nil); got["status"] != "completed" || got["rows_valid"] != float64(2) {
		t.Fatalf("import: %v", got)
	}
	mu.Lock()
	raw := finished[0].Data
	mu.Unlock()
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil || data["status"] != "completed" || data["import_id"] != id || strings.Contains(string(raw), "+254") {
		t.Fatalf("event data %s, %v", raw, err)
	}

	// The gRPC API answers for the imported numbers (task 6.5).
	conn, err := gogrpc.NewClient(cfg.GRPCAddr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	answers, err := contactsv1.NewContactsServiceClient(conn).CheckConsent(ctx, &contactsv1.CheckConsentRequest{
		TenantId: tenant.String(), Channel: "sms", Purpose: "promotional", Numbers: []string{"0700000001", "nope"},
	})
	if err != nil || len(answers.GetAnswers()) != 2 || !answers.GetAnswers()[0].GetAllowed() || answers.GetAnswers()[1].GetReason() != "invalid_number" {
		t.Fatalf("CheckConsent: %v, %v", answers, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run didn't stop")
	}
}
