package connectorprov

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestAdminClient_CreateAndRevokeRuntimeToken(t *testing.T) {
	var gotAuth string
	var gotBody RuntimeTokenSpec
	var revokedID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/runtime-tokens":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"plaintext-token","record":{"id":"rec-123"}}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/runtime-tokens/"):
			revokedID = strings.TrimPrefix(r.URL.Path, "/api/runtime-tokens/")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	client := NewAdminClient(u.Hostname(), port, "admin-secret")

	token, recordID, err := client.CreateRuntimeToken(context.Background(), RuntimeTokenSpec{
		Name: "claworc-instance-1",
	})
	if err != nil {
		t.Fatalf("CreateRuntimeToken: %v", err)
	}
	if token != "plaintext-token" {
		t.Errorf("token = %q, want plaintext-token", token)
	}
	if recordID != "rec-123" {
		t.Errorf("recordID = %q, want rec-123", recordID)
	}
	if gotAuth != "Bearer admin-secret" {
		t.Errorf("Authorization header = %q, want Bearer admin-secret", gotAuth)
	}
	if gotBody.Name != "claworc-instance-1" {
		t.Errorf("request body Name = %q, want claworc-instance-1", gotBody.Name)
	}
	if gotBody.AllowedActions == nil || gotBody.BlockedActions == nil || gotBody.AllowedProxies == nil {
		t.Errorf("expected nil slices to be normalized to empty arrays before marshal, got %+v", gotBody)
	}

	if err := client.RevokeRuntimeToken(context.Background(), recordID); err != nil {
		t.Fatalf("RevokeRuntimeToken: %v", err)
	}
	if revokedID != "rec-123" {
		t.Errorf("revoked id = %q, want rec-123", revokedID)
	}
}

func TestAdminClient_RevokeRuntimeToken_EmptyIDIsNoop(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	client := NewAdminClient(u.Hostname(), port, "admin-secret")

	if err := client.RevokeRuntimeToken(context.Background(), ""); err != nil {
		t.Fatalf("RevokeRuntimeToken with empty id should be a no-op, got: %v", err)
	}
	if called {
		t.Errorf("expected no HTTP call for an empty record ID")
	}
}

func TestAdminClient_CreateRuntimeToken_HTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized"}}`))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	client := NewAdminClient(u.Hostname(), port, "wrong-token")

	_, _, err := client.CreateRuntimeToken(context.Background(), RuntimeTokenSpec{Name: "x"})
	if err == nil {
		t.Fatal("expected an error for a 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected error to mention the 401 status, got: %v", err)
	}
}
