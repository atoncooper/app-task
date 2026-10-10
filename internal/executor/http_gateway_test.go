package executor

// Tests for the gateway-scoped executor auth header: dispatches to a URL whose
// host:port is on GatewayHosts carry GatewayHeader: GatewayApiKey (main-app
// callbacks behind APISIX key-auth); any other host never sees the credential.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestGatewayScopedAuthHeader(t *testing.T) {
	var gotKey, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("apikey")
		gotHeader = r.Header.Get("X-Task-Id")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Matching host → header attached.
	e := mustHTTP(t, HTTPOptions{
		Timeout:       5 * time.Second,
		GatewayHosts:  map[string]bool{u.Host: true},
		GatewayApiKey: "secret-key",
	})
	task := Task{ID: "gw1", Meta: map[string]any{"executor_url": srv.URL, "async": false}}
	if err := e.Handler()(context.Background(), task); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotKey != "secret-key" {
		t.Fatalf("apikey header = %q, want secret-key", gotKey)
	}
	if gotHeader != "gw1" {
		t.Fatalf("X-Task-Id = %q, want gw1", gotHeader)
	}

	// Non-matching host → no credential leaks.
	gotKey = ""
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("apikey")
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	e2 := mustHTTP(t, HTTPOptions{
		Timeout:       5 * time.Second,
		GatewayHosts:  map[string]bool{u.Host: true},
		GatewayApiKey: "secret-key",
	})
	if err := e2.Handler()(context.Background(), Task{ID: "gw2", Meta: map[string]any{"executor_url": other.URL, "async": false}}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotKey != "" {
		t.Fatalf("credential leaked to a non-gateway host: %q", gotKey)
	}

	// Empty key (legacy config) → no header even on a matching host.
	gotKey = ""
	e3 := mustHTTP(t, HTTPOptions{
		Timeout:      5 * time.Second,
		GatewayHosts: map[string]bool{u.Host: true},
	})
	if err := e3.Handler()(context.Background(), Task{ID: "gw3", Meta: map[string]any{"executor_url": srv.URL, "async": false}}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotKey != "" {
		t.Fatalf("header sent with empty GatewayApiKey: %q", gotKey)
	}
}
