package secrets

import (
	"bytes"
	"testing"
)

func TestWrapSealOpen(t *testing.T) {
	ws, _ := NewKey()
	laptop, _ := NewKey()
	other, _ := NewKey()

	sealed, err := Seal([]byte("s3cr3t"), ws.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("s3cr3t")) {
		t.Fatal("the value shows through")
	}
	wrapped, err := Wrap(ws, laptop.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unwrap(wrapped, laptop)
	if err != nil || got.String() != ws.String() {
		t.Fatalf("unwrap: %v", err)
	}
	if v, err := Open(sealed, got); err != nil || string(v) != "s3cr3t" {
		t.Fatalf("open: %q %v", v, err)
	}
	if _, err := Unwrap(wrapped, other); err == nil {
		t.Error("another device unwrapped the key")
	}
	if _, err := Open(sealed, other); err == nil {
		t.Error("another key opened the value")
	}
	if k, err := ParseKey(" " + ws.String() + "\n"); err != nil || k.String() != ws.String() {
		t.Errorf("recovery key: %v", err)
	}
	if _, err := ParseKey("nope"); err == nil {
		t.Error("bad key accepted")
	}
}
