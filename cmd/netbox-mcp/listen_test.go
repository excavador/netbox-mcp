package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireLoopback(t *testing.T) {
	t.Parallel()

	for addr, ok := range map[string]bool{
		"127.0.0.1:8081":   true,
		"127.1.2.3:8081":   true,
		"[::1]:8081":       true,
		"localhost:8081":   true,
		"0.0.0.0:8081":     false,
		":8081":            false,
		"[::]:8081":        false,
		"10.1.2.3:8081":    false,
		"192.168.0.1:8081": false,
		"netbox-mcp:8081":  false,
		"example.com:8081": false,
		"127.0.0.1":        false, // no port: not an address to listen on
		"":                 false,
	} {
		err := requireLoopback(addr)
		if ok && err != nil {
			t.Errorf("requireLoopback(%q) = %v, want nil", addr, err)
		}

		if !ok && err == nil {
			t.Errorf("requireLoopback(%q) accepted a non-loopback address", addr)
		}
	}
}

func get(h http.Handler, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	return rec.Code
}

// The MCP listener serves MCP and nothing else: no health endpoint and no
// protected-resource metadata -- those belong to the ops listener and to the
// sidecar.
func TestMCPMuxServesOnlyMCP(t *testing.T) {
	t.Parallel()

	mux := mcpMux(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	for _, p := range []string{"/mcp", "/mcp/", "/mcp/x"} {
		if got := get(mux, p); got != http.StatusTeapot {
			t.Errorf("GET %s = %d, want the MCP handler", p, got)
		}
	}

	for _, p := range []string{"/healthz", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/"} {
		if got := get(mux, p); got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, got)
		}
	}
}

// The ops listener serves /healthz and none of the MCP surface.
func TestOpsMuxServesOnlyHealthz(t *testing.T) {
	t.Parallel()

	mux := opsMux()

	if got := get(mux, "/healthz"); got != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", got)
	}

	for _, p := range []string{"/mcp", "/mcp/", "/.well-known/oauth-protected-resource", "/"} {
		if got := get(mux, p); got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, got)
		}
	}
}
