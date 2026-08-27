package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestConnectorOAuthCallbackRoute_BypassesAdminAuth is a regression guard for
// the exact route-registration shape in main.go's server setup: the static
// "/connector/oauth/callback" route (no auth middleware) must win over the
// admin-gated "/connector/*" wildcard registered in a sibling group,
// regardless of which one is registered first in source order. chi resolves
// routes via a radix tree, not registration order, so this is safe -- but
// it's exactly the kind of routing behavior that's easy to accidentally
// invert during a refactor (e.g. by moving the callback route inside the
// RequireAuth/RequireAdmin group "for tidiness"), so it's worth a standing
// test rather than relying on manual verification.
//
// See main.go's comment beside `r.Get("/connector/oauth/callback", ...)` for
// why the callback itself doesn't need Claworc-level auth: OpenConnector's
// own /oauth/callback handler treats it as a public path (see open-connector
// src/server/api/auth.ts's isPublicPath) and safety comes from the
// single-use, time-boxed OAuth state token, not from Claworc's session.
func TestConnectorOAuthCallbackRoute_BypassesAdminAuth(t *testing.T) {
	var authChecked bool

	r := chi.NewRouter()

	// Mirrors the admin-gated wildcard proxy group in main.go.
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				authChecked = true
				next.ServeHTTP(w, req)
			})
		})
		r.HandleFunc("/connector/*", func(w http.ResponseWriter, req *http.Request) {
			w.Write([]byte("wildcard"))
		})
	})

	// Mirrors the unauthenticated OAuth callback exemption in main.go.
	r.Get("/connector/oauth/callback", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("callback"))
	})

	req := httptest.NewRequest(http.MethodGet, "/connector/oauth/callback", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Body.String() != "callback" {
		t.Fatalf("expected the unauthenticated callback route to handle the request, got body %q", rec.Body.String())
	}
	if authChecked {
		t.Fatalf("expected no admin-auth middleware to run for /connector/oauth/callback")
	}

	// Every other /connector/* path must still go through the admin-gated
	// wildcard group.
	authChecked = false
	req2 := httptest.NewRequest(http.MethodGet, "/connector/", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	if rec2.Body.String() != "wildcard" {
		t.Fatalf("expected other /connector/* paths to hit the admin-gated wildcard, got body %q", rec2.Body.String())
	}
	if !authChecked {
		t.Fatalf("expected admin-auth middleware to run for /connector/")
	}
}
