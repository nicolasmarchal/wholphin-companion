package secure

import (
	"bytes"
	"testing"
)

func TestSealerRoundTripAndAssociatedData(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.Seal([]byte("tracker-secret"), []byte("candidate:payload"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := sealer.Open(ciphertext, []byte("candidate:payload"))
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "tracker-secret" {
		t.Fatalf("got %q", plain)
	}
	if _, err = sealer.Open(ciphertext, []byte("another-candidate")); err == nil {
		t.Fatal("opening with different associated data must fail")
	}
}

func TestTokenIsOpaqueAndDistinct(t *testing.T) {
	first, err := Token(32)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Token(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 32 || len(second) < 32 || first == second {
		t.Fatalf("tokens are not distinct opaque values: %q %q", first, second)
	}
}
