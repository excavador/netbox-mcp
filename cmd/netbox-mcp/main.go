// Command netbox-mcp serves a NetBox instance over the Model Context
// Protocol: DCIM and IPAM, read and written.
//
// Two transports:
//
//	stdio       the default, for running it locally next to a client
//	http        streamable HTTP, for running it in a cluster
//
// The HTTP transport holds NO authentication code. It is meant to run behind
// the sluis resource-proxy sidecar in the same pod, which verifies the
// caller's access token, serves the RFC 9728 Protected Resource Metadata and
// forwards to this server on loopback. So the MCP endpoint (/mcp) binds a
// loopback address only, and the server REFUSES a non-loopback --addr unless
// --allow-non-loopback is given: fail closed, so a stray 0.0.0.0 can never
// publish NetBox, writable, to anything that can reach the pod. A second
// listener (--ops-addr) serves /healthz for the kubelet and nothing of MCP.
// See docs/design/cimd-auth.md.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urfave/cli/v3"

	"github.com/excavador/netbox-mcp/internal/netbox"
	"github.com/excavador/netbox-mcp/internal/server"
)

const (
	// defaultAddr is where the MCP endpoint listens: loopback, for the
	// resource-proxy sidecar in the same pod.
	defaultAddr = "127.0.0.1:8081"
	// defaultOpsAddr is where the ops listener (/healthz) listens.
	defaultOpsAddr = "0.0.0.0:9090"
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
				// Loopback, and enforced: the sluis resource-proxy sidecar
				// in the same pod is the only caller, and it is the one
				// that checks the bearer token. See requireLoopback.
				Usage:   "listen address of the MCP endpoint, http transport only; must be loopback",
				Value:   defaultAddr,
				Sources: cli.EnvVars("ADDR"),
			},
			&cli.BoolFlag{
				Name: "allow-non-loopback",
				// The one way past the loopback guard. There is no
				// authentication in this process, so a non-loopback --addr
				// exposes NetBox, writable, to whoever can reach it.
				Usage:   "let --addr be a non-loopback address; this process authenticates nobody, so only behind something that does",
				Sources: cli.EnvVars("ALLOW_NON_LOOPBACK"),
			},
			&cli.StringFlag{
				Name: "ops-addr",
				// Not loopback: the kubelet's probes arrive on the pod IP.
				// It serves /healthz and nothing of the MCP surface.
				Usage:   "listen address of the ops listener (/healthz only), http transport only; empty disables it",
				Value:   defaultOpsAddr,
				Sources: cli.EnvVars("OPS_ADDR"),
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

	// Before anything else, so a wrong address fails at once and without
	// touching NetBox.
	addr := cmd.String("addr")
	if cmd.String("transport") == "http" {
		if cmd.Bool("allow-non-loopback") {
			log.Warn("--allow-non-loopback is set: this process authenticates nobody", "addr", addr)
		} else if err := requireLoopback(addr); err != nil {
			return err
		}
	}

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

	return serve(ctx, log, addr, cmd.String("ops-addr"), mcpMux(handler))
}
