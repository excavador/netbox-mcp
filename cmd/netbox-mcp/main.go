// Command netbox-mcp serves a NetBox instance over the Model Context
// Protocol: DCIM and IPAM, read and written.
//
// Two transports:
//
//	stdio       the default, for running it locally next to a client
//	http        streamable HTTP, for running it in a cluster
//
// The HTTP transport does NO authentication of its own. It is meant to sit
// behind a gateway that validates a token -- on hive that is Envoy checking a
// Zitadel-issued JWT against the netbox project audience. Exposing this
// directly to a network is exposing NetBox, writable.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"

	"github.com/excavador/netbox-mcp/internal/netbox"
	"github.com/excavador/netbox-mcp/internal/server"
)

// version is overridden at build time.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := &cli.Command{
		Name:    "netbox-mcp",
		Usage:   "MCP server for a NetBox instance",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "netbox-url",
				Usage:    "base URL of the NetBox instance, without /api",
				Sources:  cli.EnvVars("NETBOX_URL"),
				Required: true,
			},
			&cli.StringFlag{
				Name: "netbox-token",
				// A NetBox API token, sent as "Authorization: Token ...".
				// NetBox does not accept an OIDC token.
				Usage:    "NetBox API token",
				Sources:  cli.EnvVars("NETBOX_TOKEN"),
				Required: true,
			},
			&cli.StringFlag{
				Name:    "transport",
				Usage:   "stdio or http",
				Value:   "stdio",
				Sources: cli.EnvVars("TRANSPORT"),
			},
			&cli.StringFlag{
				Name: "addr",
				// 0.0.0.0, not 127.0.0.1: in a pod, loopback means nothing
				// can reach it, including the readiness probe.
				Usage:   "listen address for the http transport",
				Value:   "0.0.0.0:8080",
				Sources: cli.EnvVars("ADDR"),
			},
		},
		Action: run,
	}

	if err := cmd.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "netbox-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	client := netbox.New(cmd.String("netbox-url"), cmd.String("netbox-token"))

	// Fail here rather than on the first tool call. A wrong URL and a rejected
	// token look nothing alike in the error, which is the point.
	nbVersion, err := client.Status(ctx)
	if err != nil {
		return fmt.Errorf("netbox unreachable or token rejected: %w", err)
	}

	registry := &netbox.Registry{}
	if err := registry.Load(ctx, client); err != nil {
		return fmt.Errorf("discover object types: %w", err)
	}

	log.Info("connected to netbox",
		"netboxVersion", nbVersion, "objectTypes", len(registry.Types()))

	s := server.New(client, registry, version)

	if cmd.String("transport") != "http" {
		return s.Run(ctx, &mcp.StdioTransport{})
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/mcp/", handler)

	// Liveness only, and deliberately does NOT call NetBox: a readiness probe
	// that fails when a dependency blips takes the pod out of service for
	// something restarting cannot fix.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	addr := cmd.String("addr")
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutdown)
	}()

	log.Info("serving mcp over http", "addr", addr)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}
