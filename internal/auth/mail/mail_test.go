package mail

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"log": "log",
		"smtp://noreply%40acme.dev:s3cret@smtp.acme.dev:587": "smtp://noreply%40acme.dev:xxxxx@smtp.acme.dev:587",
		"smtp://smtp.acme.dev:25":                            "smtp://smtp.acme.dev:25",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := Parse("smtp://u:s3cret@", "a@b", nil); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error shows the password: %v", err)
	}
}
