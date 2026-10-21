package crypto

import "testing"

func TestHMAC_SignThenVerify(t *testing.T) {
	key := []byte("a shared signing secret")
	data := []byte("POST /v1/payouts\n{...}\n2026-10-21T12:00:00Z")

	sig := HMACSign(key, data)
	if !HMACVerify(key, data, sig) {
		t.Fatal("expected the correct signature to verify")
	}
}

func TestHMAC_WrongKeyFailsVerify(t *testing.T) {
	data := []byte("payload")
	sig := HMACSign([]byte("key-a"), data)
	if HMACVerify([]byte("key-b"), data, sig) {
		t.Fatal("expected verification under a different key to fail")
	}
}

func TestHMAC_WrongDataFailsVerify(t *testing.T) {
	key := []byte("key")
	sig := HMACSign(key, []byte("original payload"))
	if HMACVerify(key, []byte("tampered payload"), sig) {
		t.Fatal("expected verification against different data to fail")
	}
}

func TestHMAC_TamperedSignatureFailsVerify(t *testing.T) {
	key := []byte("key")
	data := []byte("payload")
	sig := HMACSign(key, data)
	sig[0] ^= 0xFF
	if HMACVerify(key, data, sig) {
		t.Fatal("expected a tampered signature to fail verification")
	}
}
