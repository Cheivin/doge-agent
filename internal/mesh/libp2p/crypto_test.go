//go:build mesh

package libp2p

import (
	"bytes"
	"testing"

	"doge/internal/mesh"
)

func TestDeriveKeyAndHmac(t *testing.T) {
	tok, err := mesh.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	psk, err := pskBytes(tok.PSK)
	if err != nil {
		t.Fatal(err)
	}

	// Same psk + purpose => same key; different purpose => different key.
	k1 := deriveKey(psk, "advertise")
	k2 := deriveKey(psk, "advertise")
	k3 := deriveKey(psk, "other")
	if !bytes.Equal(k1, k2) {
		t.Error("deriveKey not deterministic")
	}
	if bytes.Equal(k1, k3) {
		t.Error("deriveKey should differ across purposes")
	}
	if len(k1) != 32 {
		t.Errorf("key length = %d, want 32", len(k1))
	}

	// HMAC tokens: sender hashes the remote's ID, receiver hashes its own.
	aID, bID := "12D3KooWaaaa", "12D3KooWbbbb"
	ab := hmacToken(psk, bID) // A -> B hashes B
	ba := hmacToken(psk, aID) // B -> A hashes A
	if !hmacEqual(ab, hmacToken(psk, bID)) {
		t.Error("B should verify A's token against its own ID")
	}
	if !hmacEqual(ba, hmacToken(psk, aID)) {
		t.Error("A should verify B's token against its own ID")
	}
	if hmacEqual(ab, ba) {
		t.Error("tokens must differ between directions")
	}
	// A wrong psk produces a token that fails verification.
	if hmacEqual(ab, hmacToken([]byte("wrong"), bID)) {
		t.Error("wrong psk must not pass")
	}
}

func TestEncryptDecryptRecord(t *testing.T) {
	tok, _ := mesh.NewToken()
	psk, _ := pskBytes(tok.PSK)
	plain := []byte(`{"peerId":"12D3KooW","name":"node-a"}`)

	enc, err := encryptRecord(psk, "advertise", plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Contains(enc, plain) {
		t.Error("ciphertext leaks plaintext")
	}
	dec, err := decryptRecord(psk, "advertise", enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(dec, plain) {
		t.Errorf("round trip = %q, want %q", dec, plain)
	}

	// Wrong purpose / wrong key must fail.
	if _, err := decryptRecord(psk, "handshake", enc); err == nil {
		t.Error("expected error decrypting with the wrong purpose")
	}
	other, _ := mesh.NewToken()
	otherPSK, _ := pskBytes(other.PSK)
	if _, err := decryptRecord(otherPSK, "advertise", enc); err == nil {
		t.Error("expected error decrypting with the wrong psk")
	}
	// Tampering must fail.
	enc[0] ^= 0xff
	if _, err := decryptRecord(psk, "advertise", enc); err == nil {
		t.Error("expected error on tampered ciphertext")
	}
}

func TestPskBytesErrors(t *testing.T) {
	if _, err := pskBytes("not-hex"); err == nil {
		t.Error("expected error for non-hex psk")
	}
}
