// Command migrate-v3 imports v3 data into v4, one module's tables at
// a time (ROADMAP §5.1: "each phase that replaces v3 data writes its
// importer and dry-runs it on an anonymised v3 copy"). Today: identity
// users and profiles only (task 1.13); every later phase's own v3
// importer task gets its own subcommand here as it's built, the same
// way tools/vaspro grows one subcommand per ops action.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != "identity-users" {
		usage()
		return 2
	}
	return runIdentityUsers(args[1:], os.Stdout, os.Stderr)
}

func usage() {
	_, _ = fmt.Fprintln(os.Stderr, "usage: migrate-v3 identity-users --users <users.csv> [--profiles <profiles.csv>]")
	_, _ = fmt.Fprintln(os.Stderr, "  reads IDENTITY_DATABASE_URL from the environment")
}

func runIdentityUsers(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("identity-users", flag.ContinueOnError)
	usersPath := fs.String("users", "", "path to v3's exported user table (CSV)")
	profilesPath := fs.String("profiles", "", "path to v3's exported profile table (CSV), optional")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *usersPath == "" {
		_, _ = fmt.Fprintln(stderr, "migrate-v3: --users is required")
		return 2
	}

	dsn := os.Getenv("IDENTITY_DATABASE_URL")
	if dsn == "" {
		_, _ = fmt.Fprintln(stderr, "migrate-v3: IDENTITY_DATABASE_URL is required")
		return 2
	}

	records, err := readV3UserRecords(*usersPath, *profilesPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "migrate-v3: reading v3 export:", err)
		return 1
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "migrate-v3: connecting to the database:", err)
		return 1
	}
	defer pool.Close()

	// A plain app.Service with just a Pool and Clock: ImportV3Users
	// touches no email/KMS/WebAuthn/Redis dependency, the same minimal-
	// deps pattern tools/vaspro's rotateSigningKey already uses for an
	// ops action that doesn't need a running service's full dependency
	// graph.
	svc := app.New(app.Deps{Pool: pool, Clock: clock.Real{}})

	report, err := svc.ImportV3Users(ctx, records)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "migrate-v3: importing:", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "imported: %d  skipped: %d  flagged: %d\n", report.Imported, report.Skipped, report.Flagged)
	for _, o := range report.Outcomes {
		if o.Status == "flagged" {
			_, _ = fmt.Fprintf(stdout, "  flagged  v3_id=%s  email=%q  reason=%s\n", o.V3ID, o.Email, o.Reason)
		}
	}
	return 0
}
