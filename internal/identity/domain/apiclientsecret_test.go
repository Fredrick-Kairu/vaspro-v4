package domain_test

import (
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
)

func TestGenerateApiClientSecret_HasEnvironmentPrefixAndMatchingLast4(t *testing.T) {
	cases := []string{"sandbox", "live"}
	for _, env := range cases {
		t.Run(env, func(t *testing.T) {
			secret, last4, err := domain.GenerateApiClientSecret(env)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.HasPrefix(secret, "sk_"+env+"_") {
				t.Fatalf("secret %q doesn't have the expected prefix sk_%s_", secret, env)
			}
			if len(last4) != 4 {
				t.Fatalf("got last4 %q, want length 4", last4)
			}
			if !strings.HasSuffix(secret, last4) {
				t.Fatalf("secret %q doesn't end with last4 %q", secret, last4)
			}
		})
	}
}

func TestGenerateApiClientSecret_DiffersAcrossCalls(t *testing.T) {
	first, _, err := domain.GenerateApiClientSecret("live")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, _, err := domain.GenerateApiClientSecret("live")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == second {
		t.Fatal("expected two generated secrets to differ")
	}
}
