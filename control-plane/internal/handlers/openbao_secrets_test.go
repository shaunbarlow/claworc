package handlers

import (
	"testing"

	"github.com/gluk-w/claworc/control-plane/internal/database"
)

// The path validator is the only thing standing between a request body and a
// KV path built by string concatenation, so its rejections matter more than
// its acceptances: anything containing ".." or a leading slash must not reach
// OpenBao, or an admin could address another agent's namespace (or a shared
// set) through their own agent's endpoint.
func TestValidateSecretRelPath(t *testing.T) {
	valid := []string{
		"token",
		"github/token",
		"a/b/c/d/e/f",
		"my-secret.v2",
		"user_name~1+2@host",
	}
	for _, p := range valid {
		if !validateSecretRelPath(p) {
			t.Errorf("expected %q to be valid", p)
		}
	}

	invalid := []string{
		"",
		"/token",
		"token/",
		"a//b",
		"..",
		"../other-agent/token",
		"a/../../b",
		"a/./b",
		"a/b/c/d/e/f/g", // deeper than maxSecretListDepth
		"tok en",
		"tok/en?x=1",
		"tok%2fen",
	}
	for _, p := range invalid {
		if validateSecretRelPath(p) {
			t.Errorf("expected %q to be rejected", p)
		}
	}
}

func TestInstanceSecretBasePath(t *testing.T) {
	inst := &database.Instance{UUID: "abc-123"}
	if got, want := instanceSecretBasePath(inst), "agents/abc-123/"; got != want {
		t.Errorf("instanceSecretBasePath = %q, want %q", got, want)
	}
}

// Every value leaving the list endpoint must be masked, whatever type it was
// stored as -- a number or object written through OpenBao's raw API would
// otherwise be JSON-encoded straight into the response.
func TestMaskSecretFields(t *testing.T) {
	fields := map[string]interface{}{
		"token":    "supersecretvalue",
		"port":     8080,
		"nested":   map[string]interface{}{"a": "b"},
		"empty":    "",
		"tiny":     "ab",
		"aaa_sort": "zzzz1234",
	}
	got := maskSecretFields(fields)
	if len(got) != len(fields) {
		t.Fatalf("got %d fields, want %d", len(got), len(fields))
	}
	if got[0].Key != "aaa_sort" {
		t.Errorf("fields not sorted by key: first is %q", got[0].Key)
	}
	byKey := map[string]string{}
	for _, f := range got {
		byKey[f.Key] = f.Masked
	}
	for key, want := range map[string]string{
		"token": "****alue",
		// Mask only keeps a 4-char tail when the value is longer than
		// that, so a short value collapses to bare asterisks.
		"port":  "****",
		"empty": "",
		"tiny":  "****",
	} {
		if byKey[key] != want {
			t.Errorf("field %q masked as %q, want %q", key, byKey[key], want)
		}
	}
	if masked := byKey["nested"]; masked == `{"a":"b"}` || len(masked) > 8 {
		t.Errorf("nested value not masked: %q", masked)
	}
}
