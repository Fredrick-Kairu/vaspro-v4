package crypto

import "testing"

func TestArgon2_HashThenVerify(t *testing.T) {
	encoded, err := Argon2Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Argon2Hash: %v", err)
	}

	ok, err := Argon2Verify(encoded, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Argon2Verify: %v", err)
	}
	if !ok {
		t.Fatal("expected the correct secret to verify")
	}
}

func TestArgon2_WrongSecretFailsVerify(t *testing.T) {
	encoded, err := Argon2Hash("the-real-secret")
	if err != nil {
		t.Fatalf("Argon2Hash: %v", err)
	}

	ok, err := Argon2Verify(encoded, "not-the-real-secret")
	if err != nil {
		t.Fatalf("Argon2Verify: %v", err)
	}
	if ok {
		t.Fatal("expected a wrong secret to fail verification")
	}
}

func TestArgon2_HashIsSaltedDifferentlyEachTime(t *testing.T) {
	a, err := Argon2Hash("same secret")
	if err != nil {
		t.Fatalf("Argon2Hash: %v", err)
	}
	b, err := Argon2Hash("same secret")
	if err != nil {
		t.Fatalf("Argon2Hash: %v", err)
	}
	if a == b {
		t.Fatal("expected two hashes of the same secret to differ (random salt)")
	}

	for _, encoded := range []string{a, b} {
		ok, err := Argon2Verify(encoded, "same secret")
		if err != nil {
			t.Fatalf("Argon2Verify: %v", err)
		}
		if !ok {
			t.Fatalf("expected %q to verify", encoded)
		}
	}
}

func TestArgon2_VerifyMalformedEncodingErrors(t *testing.T) {
	cases := []string{
		"",
		"not-an-argon2-hash-at-all",
		"$argon2id$v=19$m=65536,t=1,p=4$onlyonefield",
		"$bcrypt$2a$10$somethingelse",
	}
	for _, encoded := range cases {
		if _, err := Argon2Verify(encoded, "whatever"); err == nil {
			t.Fatalf("expected malformed encoding %q to error, not panic or silently fail", encoded)
		}
	}
}

func TestArgon2_EncodingIsSelfDescribing(t *testing.T) {
	encoded, err := Argon2Hash("secret")
	if err != nil {
		t.Fatalf("Argon2Hash: %v", err)
	}
	const prefix = "$argon2id$v=19$"
	if len(encoded) < len(prefix) || encoded[:len(prefix)] != prefix {
		t.Fatalf("expected encoding to start with %q, got %q", prefix, encoded)
	}
}
