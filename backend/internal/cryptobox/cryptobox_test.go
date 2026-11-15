package cryptobox

import (
	"encoding/base64"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	box, err := New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	secret := "rtsp://user:p@ss@10.0.0.5:554/live"
	enc, err := box.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	if enc == secret || enc == "" {
		t.Fatal("secret was not encrypted")
	}
	got, err := box.Decrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("got %q, want %q", got, secret)
	}
}

func TestEmptyStaysEmpty(t *testing.T) {
	box, _ := New(make([]byte, 32))
	enc, err := box.Encrypt("")
	if err != nil || enc != "" {
		t.Fatalf("encrypt empty: %q %v", enc, err)
	}
	dec, err := box.Decrypt("")
	if err != nil || dec != "" {
		t.Fatalf("decrypt empty: %q %v", dec, err)
	}
}

func TestDecryptRejectsTampered(t *testing.T) {
	box, _ := New(make([]byte, 32))
	enc, _ := box.Encrypt("hunter2")
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	if _, err := box.Decrypt(base64.RawStdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}

func TestNewRejectsBadKeySize(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}
