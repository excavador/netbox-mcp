package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// requireLoopback refuses any listen address that is not a loopback address.
//
// This process holds no authentication code: the sluis resource-proxy
// sidecar in front of it verifies every token. So an address that other
// pods can reach would publish NetBox, writable, to them with no check at
// all. Fail closed, and decide from the address as written -- no name
// resolution (a name can resolve differently at start than at review).
// Accepted: localhost, 127.0.0.0/8, ::1. Refused: an empty host (":8081"
// listens on every interface), 0.0.0.0, ::, and any other name or address.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--addr %q: %w", addr, err)
	}

	if host == "localhost" {
		return nil
	}

	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}

	return fmt.Errorf("--addr %q is not a loopback address: this server authenticates nobody and is meant to sit behind the resource-proxy sidecar, so it listens on 127.0.0.1 only (set --allow-non-loopback / ALLOW_NON_LOOPBACK to override)", addr)
}

// mcpMux serves MCP at /mcp and nothing else: no health endpoint, no
// protected-resource metadata -- the sidecar answers those.
func mcpMux(handler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/mcp/", handler)

	return mux
}

// opsMux serves liveness for the kubelet and nothing of the MCP surface.
//
// /healthz deliberately does NOT call NetBox: a probe that fails when a
// dependency blips takes the pod out of service for something restarting
// cannot fix. It is unauthenticated on purpose -- a probe carries no token --
// and that is safe because it discloses nothing.
func opsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	return mux
}

// serve runs the MCP listener and, when opsAddr is set, the ops listener,
// until ctx ends or either fails.
func serve(ctx context.Context, log *slog.Logger, addr, opsAddr string, mcpHandler http.Handler) error {
	servers := []*http.Server{newServer(addr, mcpHandler)}
	if opsAddr != "" {
		servers = append(servers, newServer(opsAddr, opsMux()))
	}

	errc := make(chan error, len(servers))

	for _, srv := range servers {
		log.Info("serving http", "addr", srv.Addr)

		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("listen %s: %w", srv.Addr, err)

				return
			}

			errc <- nil
		}()
	}

	var first error

	select {
	case <-ctx.Done():
	case first = <-errc:
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, srv := range servers {
		_ = srv.Shutdown(shutdown)
	}

	return first
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
}
