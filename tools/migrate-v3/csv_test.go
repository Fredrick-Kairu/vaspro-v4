package main

import "testing"

func TestReadV3UserRecords_JoinsProfilesByUserID(t *testing.T) {
	records, err := readV3UserRecords("testdata/users.csv", "testdata/profiles.csv")
	if err != nil {
		t.Fatalf("readV3UserRecords: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}

	byV3ID := make(map[string]string)
	for _, r := range records {
		byV3ID[r.V3ID] = r.Msisdn
	}
	if byV3ID["1"] != "0712345678" {
		t.Fatalf("got msisdn %q for v3 id 1, want 0712345678", byV3ID["1"])
	}
	if byV3ID["2"] != "" {
		t.Fatalf("got msisdn %q for v3 id 2 (no profile row), want empty", byV3ID["2"])
	}
	if byV3ID["3"] != "0712345679" {
		t.Fatalf("got msisdn %q for v3 id 3, want 0712345679", byV3ID["3"])
	}
}

func TestReadV3UserRecords_NoProfilesFile_StillReadsUsers(t *testing.T) {
	records, err := readV3UserRecords("testdata/users.csv", "")
	if err != nil {
		t.Fatalf("readV3UserRecords: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	for _, r := range records {
		if r.Msisdn != "" {
			t.Fatalf("got msisdn %q with no profiles file, want empty", r.Msisdn)
		}
	}
}

func TestReadV3UserRecords_MissingUsersFile_Errors(t *testing.T) {
	if _, err := readV3UserRecords("testdata/does-not-exist.csv", ""); err == nil {
		t.Fatal("expected an error for a missing users file")
	}
}
