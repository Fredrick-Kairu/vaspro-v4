//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

// sampleCSV has, by spreadsheet row (the header is row 1):
//
//	2 a local number            3 the same number again (later wins)
//	4 254… with an attribute    5 a landline (refused)
//	6 a blank row (skipped)     7 a bad attribute value (refused)
//	8 +254… quoted, with a comma in the name
var sampleCSV = "\ufeffPhone,First Name,Surname,County,Notes\n" +
	"0700000001,Ann,Kim,Nairobi,x\n" +
	"0700 000 001,Anne,,,\n" +
	"254100000002,Ben,,Mombasa,\n" +
	"0200000003,Cat,,,\n" +
	",,,,\n" +
	"0700000004,Dan,,Nairobi City County with a name far too long for the attribute " + strings.Repeat("x", 250) + ",\n" +
	"\"+254 700 000 005\",\"Eve, Jr\",,,\n"

func defineCounty(t *testing.T, h *harness, m member) {
	t.Helper()
	if _, err := h.svc.CreateAttribute(context.Background(), m.user, m.tenant, domain.AttributeDef{Key: "county", Label: "County", Type: "text"}); err != nil {
		t.Fatal(err)
	}
}

func TestImport_CSVEndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	defineCounty(t, h, m)
	g, err := h.svc.CreateGroup(ctx, m.user, m.tenant, "Imported", "")
	if err != nil {
		t.Fatal(err)
	}
	// An existing contact the import merges into.
	if _, _, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{Phone: "+254100000002", Fields: domain.ContactFields{Email: "ben@example.com"}}); err != nil {
		t.Fatal(err)
	}

	in := csvImport()
	in.GroupID = &g.ID
	imp, form, err := h.svc.CreateImport(ctx, m.user, m.tenant, in)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Status != domain.AwaitingUpload || !imp.UploadExpiresAt.Equal(h.clk.Now().Add(15*time.Minute)) ||
		!imp.DeleteAfter.Equal(imp.UploadExpiresAt.Add(48*time.Hour)) || form.MaxBytes != 20<<20 {
		t.Fatalf("a new import: %+v, form %+v", imp, form)
	}
	if strings.Contains(form.Form.Fields["key"], "contacts.csv") || !strings.HasPrefix(form.Form.Fields["key"], "quarantine/"+m.tenant.String()+"/") {
		t.Fatalf("the upload key %q isn't built from IDs only", form.Form.Fields["key"])
	}
	upload(t, form.Form, []byte(sampleCSV))
	confirmed, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID)
	if err != nil || confirmed.Status != domain.Scanning {
		t.Fatalf("confirm: %+v, %v", confirmed, err)
	}
	if _, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID); !errors.Is(err, app.ErrImportState) {
		t.Fatalf("confirming twice: %v", err)
	}
	h.drain(t)

	got, err := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.CompletedWithErrors || got.RowsValid != 4 || got.RowsFailed != 2 || got.RowsTotal == nil || *got.RowsTotal != 7 {
		t.Fatalf("finished import: status %s, valid %d, failed %d, total %v", got.Status, got.RowsValid, got.RowsFailed, got.RowsTotal)
	}
	if got.FinishedAt == nil || !got.DeleteAfter.Equal(got.FinishedAt.Add(48*time.Hour)) || !got.HasErrorReport || got.SizeBytes == nil || *got.SizeBytes != int64(len(sampleCSV)) {
		t.Fatalf("finished import: %+v", got)
	}

	t.Run("contacts merged as the rules say", func(t *testing.T) {
		list, _, err := h.svc.ListContacts(ctx, m.user, m.tenant, nil, app.Page{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		byPhone := map[string]app.Contact{}
		for _, c := range list {
			byPhone[c.Phone] = c
		}
		if len(byPhone) != 3 {
			t.Fatalf("%d contacts: %v", len(byPhone), byPhone)
		}
		a := byPhone["+254700000001"]
		// Row 3 wins where it has values; row 2's surname and county stay.
		if a.FirstName != "Anne" || a.LastName != "Kim" || string(a.Attributes["county"]) != `"Nairobi"` || a.Source != "import" {
			t.Errorf("+254700000001: %+v", a)
		}
		b := byPhone["+254100000002"]
		if b.FirstName != "Ben" || b.Email != "ben@example.com" || b.Source != "api" {
			t.Errorf("the existing contact, merged: %+v", b)
		}
		if e := byPhone["+254700000005"]; e.FirstName != "Eve, Jr" {
			t.Errorf("the quoted row: %+v", e)
		}
		members, _, err := h.svc.ListContacts(ctx, m.user, m.tenant, &g.ID, app.Page{})
		if err != nil || len(members) != 3 {
			t.Errorf("group members: %d, %v", len(members), err)
		}
	})

	t.Run("errors by row, naming the problem, never the value", func(t *testing.T) {
		errs, next, err := h.svc.ListImportErrors(ctx, m.user, m.tenant, imp.ID, app.Page{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		more, _, err := h.svc.ListImportErrors(ctx, m.user, m.tenant, imp.ID, app.Page{Cursor: next})
		if err != nil {
			t.Fatal(err)
		}
		errs = append(errs, more...)
		want := []app.ImportError{
			{Row: 1, Field: "Notes", Code: "VAL-4021"},
			{Row: 5, Field: "phone", Code: "VAL-4020"},
			{Row: 7, Field: "county", Code: "VAL-4021"},
		}
		if len(errs) != len(want) {
			t.Fatalf("errors: %+v", errs)
		}
		for i, w := range want {
			if errs[i].Row != w.Row || errs[i].Field != w.Field || errs[i].Code != w.Code || errs[i].Msg == "" {
				t.Errorf("error %d: %+v, want %+v", i, errs[i], w)
			}
			if strings.Contains(errs[i].Msg, "0200000003") || strings.Contains(errs[i].Msg, "Nairobi City") {
				t.Errorf("error %d repeats the row's value: %q", i, errs[i].Msg)
			}
		}
	})

	t.Run("the error report and the original download", func(t *testing.T) {
		link, expires, err := h.svc.CreateDownloadLink(ctx, m.user, m.tenant, imp.ID, true)
		if err != nil || !strings.HasPrefix(link, "https://api.vaspro.test/v1/contacts/downloads/") || !expires.Equal(h.clk.Now().Add(5*time.Minute)) {
			t.Fatalf("link %q, %v, %v", link, expires, err)
		}
		report := download(t, h, link)
		if report.ContentType != "text/csv" || !strings.HasPrefix(string(report.body), "row,field,code,message\n1,Notes,VAL-4021,") ||
			!strings.Contains(string(report.body), "\n5,phone,VAL-4020,") || strings.Contains(string(report.body), "0200000003") {
			t.Fatalf("report %s:\n%s", report.ContentType, report.body)
		}
		link, _, err = h.svc.CreateDownloadLink(ctx, m.user, m.tenant, imp.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if orig := download(t, h, link); string(orig.body) != sampleCSV || orig.FileName != "contacts.csv" {
			t.Fatalf("the original: %q (%s)", orig.body, orig.FileName)
		}
		stored, err := h.store.Get(ctx, uploads.FileKey(uploads.Transient, m.tenant, imp.ID))
		if err != nil || bytes.Contains(stored, []byte("Mombasa")) {
			t.Fatalf("the stored file is missing or in plaintext: %v", err)
		}
	})

	t.Run("contact_import.finished, without numbers", func(t *testing.T) {
		evs := h.events(t, "contact_import.finished")
		if len(evs) != 1 || evs[0]["status"] != "completed_with_errors" || evs[0]["rows_valid"] != float64(4) || evs[0]["import_id"] != imp.ID.String() {
			t.Fatalf("events: %+v", evs)
		}
	})

	t.Run("another account can't reach it", func(t *testing.T) {
		other := h.manager()
		if _, _, err := h.svc.CreateDownloadLink(ctx, other.user, other.tenant, imp.ID, false); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("another account's link: %v", err)
		}
		if _, err := h.svc.GetImport(ctx, other.user, other.tenant, imp.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("another account's read: %v", err)
		}
	})

	if logs := h.logs.String(); strings.Contains(logs, "700000001") || strings.Contains(logs, "Mombasa") {
		t.Fatal("row data reached the logs")
	}
}

type downloaded struct {
	app.Download
	body []byte
}

func download(t *testing.T, h *harness, link string) downloaded {
	t.Helper()
	token := link[strings.LastIndex(link, "/")+1:]
	d, err := h.svc.OpenDownload(context.Background(), token)
	if err != nil {
		t.Fatalf("OpenDownload: %v", err)
	}
	body, err := io.ReadAll(d.Body)
	_ = d.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return downloaded{Download: d, body: body}
}

func TestImport_XLSX(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	defineCounty(t, h, m)
	f := excelize.NewFile()
	rows := [][]any{{"MSISDN", "Name", "County"}, {"0700000001", "Ann", "Nairobi"}, {254100000002, "Ben", nil}, {"not a number", "Cat", nil}}
	for i, r := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		if err := f.SetSheetRow("Sheet1", cell, &r); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	imp := h.runImport(t, m, app.ImportInput{FileName: "contacts.xlsx", ContentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}, buf.Bytes())
	if imp.Status != domain.CompletedWithErrors || imp.RowsValid != 2 || imp.RowsFailed != 1 {
		t.Fatalf("XLSX import: %s, %d valid, %d failed", imp.Status, imp.RowsValid, imp.RowsFailed)
	}
}

func TestImport_DuplicatesAcrossImportsMerge(t *testing.T) {
	h := newHarness(t)
	m := h.manager()
	first := h.runImport(t, m, csvImport(), []byte("phone,first_name\n0700000001,Ann\n0700000002,Ben\n"))
	second := h.runImport(t, m, csvImport(), []byte("phone,last_name\n+254700000002,Test\n0700000003,\n"))
	if first.Status != domain.Completed || second.Status != domain.Completed || second.RowsValid != 2 {
		t.Fatalf("imports: %+v / %+v", first, second)
	}
	if n := h.count(t, `SELECT count(*) FROM contacts`); n != 3 {
		t.Fatalf("%d contacts after two imports sharing a number, want 3", n)
	}
	if n := h.count(t, `SELECT count(*) FROM contacts WHERE first_name = 'Ben' AND last_name = 'Test'`); n != 1 {
		t.Fatal("the shared number didn't merge both imports' fields")
	}
}

// TestImport_FileFailures: each failure is final, with a row-0 error
// whose code the event repeats, and nothing written.
func TestImport_FileFailures(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 64)...)
	for name, tc := range map[string]struct {
		body []byte
		code string
	}{
		"infected":           {append([]byte("phone\n0700000001\n"), clamd.EICAR()...), "VAL-4009"},
		"a PNG named .csv":   {png, "VAL-4007"},
		"no number column":   {[]byte("name,city\nAnn,Nairobi\n"), "VAL-4022"},
		"two number columns": {[]byte("phone,mobile\n0700000001,0700000002\n"), "VAL-4022"},
		"not UTF-8 text":     {[]byte("phone,name\n0700000001,Jos\xe9\n"), ""},
		"an empty sheet":     {[]byte("\n"), "VAL-4022"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			m := h.manager()
			imp := h.runImport(t, m, csvImport(), tc.body)
			if tc.code == "" { // a bad row, not a bad file
				if imp.Status != domain.CompletedWithErrors || imp.RowsFailed != 1 {
					t.Fatalf("%s: %+v", name, imp)
				}
				return
			}
			if imp.Status != domain.Failed || imp.RowsValid != 0 {
				t.Fatalf("%s: %s, %d valid", name, imp.Status, imp.RowsValid)
			}
			errs, _, err := h.svc.ListImportErrors(context.Background(), m.user, m.tenant, imp.ID, app.Page{})
			if err != nil || len(errs) == 0 || errs[0].Code != tc.code || errs[0].Row > 1 {
				t.Fatalf("%s: errors %+v, %v", name, errs, err)
			}
			if evs := h.events(t, "contact_import.finished"); len(evs) != 1 || evs[0]["status"] != "failed" || evs[0]["error_code"] != tc.code {
				t.Fatalf("%s: events %+v", name, evs)
			}
			if n := h.count(t, `SELECT count(*) FROM contacts`); n != 0 {
				t.Fatalf("%s: %d contacts written", name, n)
			}
		})
	}
}

func TestImport_NothingUploaded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	imp, _, err := h.svc.CreateImport(ctx, m.user, m.tenant, csvImport())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID); err != nil {
		t.Fatal(err)
	}
	h.drain(t)
	got, _ := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	if got.Status != domain.Failed {
		t.Fatalf("confirmed with no file: %s", got.Status)
	}
}

func TestImport_ScannerDownWaitsThenGoesOn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	imp, form, err := h.svc.CreateImport(ctx, m.user, m.tenant, csvImport())
	if err != nil {
		t.Fatal(err)
	}
	upload(t, form.Form, []byte("phone\n0700000001\n"))
	if _, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID); err != nil {
		t.Fatal(err)
	}
	addr := h.clamd.Addr()
	h.clamd.Close()
	if _, err := h.svc.ProcessImports(ctx); err != nil {
		t.Fatalf("a pass with clamd down: %v", err)
	}
	if got, _ := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID); got.Status != domain.Scanning {
		t.Fatalf("with clamd down: %s, want still scanning", got.Status)
	}
	// Even far past delete_after, an import still being worked on isn't swept.
	h.clk.Advance(30 * 24 * time.Hour)
	if res, err := h.svc.SweepImportFiles(ctx); err != nil || res.Deleted != 0 {
		t.Fatalf("swept an import still scanning: %+v, %v", res, err)
	}
	h.clk.Set(time.Now().UTC())
	back, err := clamd.StartAt(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(back.Close)
	h.drain(t)
	if got, _ := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID); got.Status != domain.Completed {
		t.Fatalf("once clamd is back: %s", got.Status)
	}
}

func TestImport_ConfirmAfterTheFormExpired(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	imp, _, err := h.svc.CreateImport(ctx, m.user, m.tenant, csvImport())
	if err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(15 * time.Minute)
	if _, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID); !errors.Is(err, app.ErrImportState) {
		t.Fatalf("confirming at the form's end: %v", err)
	}
	n, err := h.svc.ExpireImports(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ExpireImports: %d, %v", n, err)
	}
	got, _ := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	if got.Status != domain.Expired || got.FinishedAt == nil || !got.DeleteAfter.Equal(got.FinishedAt.Add(48*time.Hour)) {
		t.Fatalf("expired: %+v", got)
	}
	if evs := h.events(t, "contact_import.finished"); len(evs) != 1 || evs[0]["status"] != "expired" {
		t.Fatalf("events: %+v", evs)
	}
	if n, _ := h.svc.ExpireImports(ctx); n != 0 {
		t.Fatalf("a second expiry pass expired %d", n)
	}
}

// TestImport_FilesDeletedTwoDaysAfter is data model §8's retention.
func TestImport_FilesDeletedTwoDaysAfter(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	imp := h.runImport(t, m, csvImport(), []byte("phone\n0700000001\n0200000002\n"))
	if imp.Status != domain.CompletedWithErrors {
		t.Fatalf("import: %s", imp.Status)
	}
	keys := []string{uploads.FileKey(uploads.Transient, m.tenant, imp.ID), uploads.ErrorReportKey(uploads.Transient, m.tenant, imp.ID)}
	exists := func(k string) bool { _, err := h.store.Stat(ctx, k); return err == nil }

	h.clk.Set(imp.FinishedAt.Add(48*time.Hour - time.Second))
	if res, err := h.svc.SweepImportFiles(ctx); err != nil || res.Deleted != 0 || !exists(keys[0]) || !exists(keys[1]) {
		t.Fatalf("a second early: %+v, %v", res, err)
	}
	h.clk.Set(imp.FinishedAt.Add(48 * time.Hour))
	// Due but not yet swept: no new link is signed.
	if _, _, err := h.svc.CreateDownloadLink(ctx, m.user, m.tenant, imp.ID, false); !errors.Is(err, app.ErrFileDeleted) {
		t.Fatalf("a link at delete_after, before the sweep: %v", err)
	}
	if res, err := h.svc.SweepImportFiles(ctx); err != nil || res.Deleted != 1 {
		t.Fatalf("at delete_after: %+v, %v", res, err)
	}
	if exists(keys[0]) || exists(keys[1]) {
		t.Fatal("a file survived the sweep")
	}
	got, err := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	if err != nil || got.DeletedAt == nil || !got.DeletedAt.Equal(h.clk.Now()) || got.RowsValid != 1 || got.RowsFailed != 1 {
		t.Fatalf("after the sweep: %+v, %v", got, err)
	}
	if errs, _, _ := h.svc.ListImportErrors(ctx, m.user, m.tenant, imp.ID, app.Page{}); len(errs) != 1 {
		t.Fatalf("the errors went with the files: %+v", errs)
	}
	if _, _, err := h.svc.CreateDownloadLink(ctx, m.user, m.tenant, imp.ID, false); !errors.Is(err, app.ErrFileDeleted) {
		t.Fatalf("a link after deletion: %v", err)
	}
	if res, err := h.svc.SweepImportFiles(ctx); err != nil || res.Deleted != 0 {
		t.Fatalf("a second sweep: %+v, %v", res, err)
	}
}

// TestImport_LinkMadeBeforeDeletionStopsWorking: a link signed just
// before the files go answers VAL-4024 after.
func TestImport_LinkMadeBeforeDeletionStopsWorking(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	imp := h.runImport(t, m, csvImport(), []byte("phone\n0700000001\n"))
	h.clk.Set(imp.FinishedAt.Add(48*time.Hour - time.Minute))
	link, _, err := h.svc.CreateDownloadLink(ctx, m.user, m.tenant, imp.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	h.clk.Set(imp.FinishedAt.Add(48 * time.Hour))
	if _, err := h.svc.SweepImportFiles(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.OpenDownload(ctx, link[strings.LastIndex(link, "/")+1:]); !errors.Is(err, app.ErrFileDeleted) {
		t.Fatalf("a link after the sweep: %v", err)
	}
	for _, bad := range []string{"", "abc.def", link[strings.LastIndex(link, "/")+1:] + "x"} {
		if _, err := h.svc.OpenDownload(ctx, bad); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
}

// TestImport_ResumesAfterAWorkerDies: a worker stopped after its second
// batch leaves the import processing; the next pass carries on from the
// rows already counted, and the result equals one uninterrupted run.
func TestImport_ResumesAfterAWorkerDies(t *testing.T) {
	var dies atomic.Bool
	dies.Store(true)
	h := newHarness(t, batchSize(3), afterBatch(func(_ uuid.UUID, batch int) error {
		if batch == 2 && dies.Load() {
			return errors.New("the worker died")
		}
		return nil
	}))
	m := h.manager()
	var b strings.Builder
	b.WriteString("phone,first_name\n")
	for i := range 10 {
		if i == 4 {
			b.WriteString("0200000000,Landline\n")
			continue
		}
		fmt.Fprintf(&b, "07000000%02d,P%d\n", i, i)
	}
	ctx := context.Background()
	imp, form, err := h.svc.CreateImport(ctx, m.user, m.tenant, csvImport())
	if err != nil {
		t.Fatal(err)
	}
	upload(t, form.Form, []byte(b.String()))
	if _, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = h.svc.ProcessImports(ctx) // dies after batch 2
	mid, _ := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	if mid.Status != domain.Processing || mid.RowsValid+mid.RowsFailed != 6 {
		t.Fatalf("after the worker died: %s, %d+%d", mid.Status, mid.RowsValid, mid.RowsFailed)
	}
	dies.Store(false)
	h.drain(t)
	got, _ := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	if got.Status != domain.CompletedWithErrors || got.RowsValid != 9 || got.RowsFailed != 1 || *got.RowsTotal != 10 {
		t.Fatalf("resumed: %s, %d valid, %d failed, %v total", got.Status, got.RowsValid, got.RowsFailed, *got.RowsTotal)
	}
	if n := h.count(t, `SELECT count(*) FROM contacts`); n != 9 {
		t.Fatalf("%d contacts", n)
	}
	if n := h.count(t, `SELECT count(*) FROM import_errors`); n != 1 {
		t.Fatalf("%d errors", n)
	}
}

// TestImport_TwoWorkersProcessEachImportOnce: the advisory lock.
func TestImport_TwoWorkersProcessEachImportOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, batchSize(5))
	m := h.manager()
	var ids []uuid.UUID
	for n := range 4 {
		var b strings.Builder
		b.WriteString("phone\n")
		for i := range 40 {
			fmt.Fprintf(&b, "07000%05d\n", n*100+i)
		}
		imp, form, err := h.svc.CreateImport(ctx, m.user, m.tenant, csvImport())
		if err != nil {
			t.Fatal(err)
		}
		upload(t, form.Form, []byte(b.String()))
		if _, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, imp.ID)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := h.svc.ProcessImports(ctx); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	h.drain(t)
	for _, id := range ids {
		got, _ := h.svc.GetImport(ctx, m.user, m.tenant, id)
		if got.Status != domain.Completed || got.RowsValid != 40 || *got.RowsTotal != 40 {
			t.Errorf("import %s: %s, %d valid, %v total", id, got.Status, got.RowsValid, *got.RowsTotal)
		}
	}
	if evs := h.events(t, "contact_import.finished"); len(evs) != 4 {
		t.Fatalf("%d finished events, want 4", len(evs))
	}
}

// TestPhase6ExitGate_MillionRowImport is Phase 6's exit-gate check: a
// 1,000,000-row import through a signed upload with flat memory, and its
// file gone 2 days later (fake clock). Short rows (a number and a name,
// about 17 bytes) fit the 20 MB cap. The numbers use country code 999,
// which no country has.
func TestPhase6ExitGate_MillionRowImport(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	var b bytes.Buffer
	b.WriteString("phone,name\n")
	for i := range domain.MaxImportRows {
		fmt.Fprintf(&b, "+999%08d,Ann\n", i)
	}
	if b.Len() > 20<<20 {
		t.Fatalf("the file is %d bytes, over the cap", b.Len())
	}
	imp, form, err := h.svc.CreateImport(ctx, m.user, m.tenant, csvImport())
	if err != nil {
		t.Fatal(err)
	}
	upload(t, form.Form, b.Bytes())
	if _, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID); err != nil {
		t.Fatal(err)
	}
	body := b.Len()
	uploaded := sha256.Sum256(b.Bytes())
	b = bytes.Buffer{} // let the test's own copy go before measuring
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > peak.Load() {
					peak.Store(ms.HeapAlloc)
				}
			}
		}
	}()
	began := time.Now()
	h.drain(t)
	took := time.Since(began)
	close(stop)
	<-done
	got, _ := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	growth := int64(peak.Load()) - int64(base.HeapAlloc) //nolint:gosec // G115: heap sizes
	t.Logf("imported %d rows (%.1f MB) in %s; peak heap growth %.1f MB", got.RowsValid, float64(body)/(1<<20), took.Round(time.Second), float64(growth)/(1<<20))
	if got.Status != domain.Completed || got.RowsValid != domain.MaxImportRows {
		t.Fatalf("%s, %d valid, %d failed", got.Status, got.RowsValid, got.RowsFailed)
	}
	if took > 5*time.Minute {
		t.Errorf("took %s; the target is 5 minutes", took)
	}
	if growth > 64<<20 {
		t.Errorf("peak heap grew %d MB; the target is under 64 MB", growth>>20)
	}
	if n := h.count(t, `SELECT count(*) FROM contacts`); n != domain.MaxImportRows {
		t.Fatalf("%d contacts", n)
	}

	// The stored file is the upload, sealed: its download, streamed and
	// decrypted, has the upload's SHA-256. The quarantine copy is gone.
	key := uploads.FileKey(uploads.Transient, m.tenant, imp.ID)
	exists := func(k string) bool { _, err := h.store.Stat(ctx, k); return err == nil }
	if !exists(key) || exists(uploads.QuarantineKey(m.tenant, imp.ID)) {
		t.Fatal("the stored file, or the quarantine copy's removal")
	}
	link, _, err := h.svc.CreateDownloadLink(ctx, m.user, m.tenant, imp.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := h.svc.OpenDownload(ctx, link[strings.LastIndex(link, "/")+1:])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	_, err = io.Copy(sum, dl.Body)
	_ = dl.Body.Close()
	if err != nil || !bytes.Equal(sum.Sum(nil), uploaded[:]) {
		t.Fatalf("the download isn't the upload: %v", err)
	}

	// Two days after it finished, and not a second before, the file goes;
	// the import, its counts and the contacts stay.
	h.clk.Set(got.FinishedAt.Add(48*time.Hour - time.Second))
	if res, err := h.svc.SweepImportFiles(ctx); err != nil || res.Deleted != 0 || !exists(key) {
		t.Fatalf("a second early: %+v, %v", res, err)
	}
	h.clk.Set(got.FinishedAt.Add(48 * time.Hour))
	if res, err := h.svc.SweepImportFiles(ctx); err != nil || res.Deleted != 1 || exists(key) {
		t.Fatalf("at 2 days: %+v, %v, the file still there: %v", res, err, exists(key))
	}
	after, err := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	if err != nil || after.DeletedAt == nil || after.RowsValid != domain.MaxImportRows || after.Status != domain.Completed {
		t.Fatalf("the import after deletion: %+v, %v", after, err)
	}
	if _, _, err := h.svc.CreateDownloadLink(ctx, m.user, m.tenant, imp.ID, false); !errors.Is(err, app.ErrFileDeleted) {
		t.Fatalf("a link after deletion: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM contacts`); n != domain.MaxImportRows {
		t.Fatalf("%d contacts after the file went", n)
	}
}

// TestImport_OverTheRowLimit: rows past the limit aren't read, and a
// row-0 error says so.
func TestImport_OverTheRowLimit(t *testing.T) {
	h := newHarness(t, maxRows(3))
	m := h.manager()
	imp := h.runImport(t, m, csvImport(), []byte("phone\n0700000001\n0700000002\n0700000003\n0700000004\n0700000005\n"))
	if imp.Status != domain.CompletedWithErrors || imp.RowsValid != 3 || *imp.RowsTotal != 3 {
		t.Fatalf("over the limit: %s, %d valid, %v total", imp.Status, imp.RowsValid, *imp.RowsTotal)
	}
	errs, _, _ := h.svc.ListImportErrors(context.Background(), m.user, m.tenant, imp.ID, app.Page{})
	if len(errs) != 1 || errs[0].Row != 0 || errs[0].Code != "VAL-4022" {
		t.Fatalf("errors: %+v", errs)
	}
	exact := newHarness(t, maxRows(3))
	em := exact.manager()
	if imp := exact.runImport(t, em, csvImport(), []byte("phone\n0700000001\n0700000002\n0700000003\n")); imp.Status != domain.Completed {
		t.Fatalf("exactly at the limit: %s", imp.Status)
	}
}
