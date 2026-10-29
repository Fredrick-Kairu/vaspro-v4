package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// readV3UserRecords builds the records ImportV3Users wants from v3's
// own user/profile tables, exported as two CSV files with v3's own
// column names (data model §3's "From v3" table) — the input shape
// this importer is built against; see docs/decisions/open-questions.md
// for why (no real v3 export access exists to confirm it against).
//
// users.csv header: id,email_address,full_names,password
// profiles.csv header: user_id,msisdn,network ("network" is read and
// discarded — it has nowhere to go in identity.users, data model §3's
// own mapping note).
//
// profilesPath may be empty: a users-only run still imports accounts,
// just with no phone_e164.
func readV3UserRecords(usersPath, profilesPath string) ([]app.V3UserRecord, error) {
	users, err := readCSV(usersPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", usersPath, err)
	}

	msisdnByUserID := map[string]string{}
	if profilesPath != "" {
		profiles, err := readCSV(profilesPath)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", profilesPath, err)
		}
		for _, row := range profiles {
			msisdnByUserID[row["user_id"]] = row["msisdn"]
		}
	}

	records := make([]app.V3UserRecord, 0, len(users))
	for _, row := range users {
		records = append(records, app.V3UserRecord{
			V3ID:         row["id"],
			EmailAddress: row["email_address"],
			FullNames:    row["full_names"],
			Password:     row["password"],
			Msisdn:       msisdnByUserID[row["id"]],
		})
	}
	return records, nil
}

// readCSV reads path as a header row plus data rows, returning each
// data row as a map keyed by its header's column names — small
// enough not to need a CSV library beyond the standard one, and
// tolerant of column order/extra columns either file might carry.
func readCSV(path string) ([]map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-supplied CLI flag, not untrusted network input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}

	var rows []map[string]string
	for {
		fields, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading row %d: %w", len(rows)+2, err)
		}
		row := make(map[string]string, len(header))
		for i, col := range header {
			if i < len(fields) {
				row[col] = fields[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
