// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Command nomad-mcp-server serves a Model Context Protocol interface to a
// HashiCorp Nomad cluster.
//
// It speaks two transports. `stdio` is the default and is what desktop MCP
// clients launch as a subprocess. `streamable-http` serves the same MCP server
// over HTTP for clients that connect to a long-running process.
package main

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
	"github.com/suyash1603/nomad-mcp-server/pkg/prompts"
	"github.com/suyash1603/nomad-mcp-server/pkg/resources"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
	"github.com/suyash1603/nomad-mcp-server/version"

	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	rootCmd = &cobra.Command{
		Use:   "nomad-mcp-server",
		Short: "Nomad MCP Server",
		Long: `A Model Context Protocol server that gives AI clients safe, structured
access to a HashiCorp Nomad cluster.

Configuration comes from the standard NOMAD_* environment variables, so a shell
that can already run "nomad status" can run this server unchanged. Every setting
also has a flag, and a flag always wins over the environment.

Mutating tools are refused by default. Pass --read-only=false to allow writes.`,
		Version: fmt.Sprintf("Version: %s\nCommit: %s\nBuild Date: %s",
			version.GetHumanVersion(), version.GitCommit, version.BuildDate),
		// Errors are reported by Execute()'s caller; cobra should not also
		// dump usage text on a runtime failure.
		SilenceUsage: true,
		RunE:         runDefaultCommand,
	}

	stdioCmd = &cobra.Command{
		Use:   "stdio",
		Short: "Start a stdio server",
		Long: `Start a server that communicates over stdin and stdout using JSON-RPC.

This is the transport desktop MCP clients use when they launch the server as a
subprocess.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, logger, err := setup()
			if err != nil {
				return err
			}
			return runStdioServer(cfg, logger)
		},
	}

	streamableHTTPCmd = &cobra.Command{
		Use:   "streamable-http",
		Short: "Start a StreamableHTTP server",
		Long: `Start a server that communicates using the StreamableHTTP transport.

This mode lets clients interact with the Nomad MCP server over HTTP. Use
--transport-host, --transport-port and --mcp-endpoint to control where it
listens. TLS is required unless the server is bound to localhost.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, logger, err := setup()
			if err != nil {
				return err
			}
			return runHTTPServer(cfg, logger)
		},
	}

	// httpCmdAlias keeps parity with vault-mcp-server, which named this
	// command "http" before renaming it and kept the old name working.
	httpCmdAlias = &cobra.Command{
		Use:          "http",
		Short:        "Start a StreamableHTTP server (deprecated, use 'streamable-http')",
		Long:         `Deprecated. Use "streamable-http" instead.`,
		Deprecated:   "use 'streamable-http' instead",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return streamableHTTPCmd.RunE(cmd, args)
		},
	}
)

// setup resolves configuration and builds the logger. Every command starts this
// way, so a configuration error is reported once, before anything binds a port
// or writes to stdout.
func setup() (*config.Config, *log.Logger, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	logger, err := initLogger(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cfg, logger, nil
}

// runDefaultCommand runs when no subcommand is given, dispatching on the
// resolved transport mode.
func runDefaultCommand(_ *cobra.Command, _ []string) error {
	cfg, logger, err := setup()
	if err != nil {
		return err
	}
	if cfg.IsHTTPMode() {
		return runHTTPServer(cfg, logger)
	}
	return runStdioServer(cfg, logger)
}

// NewServer builds the MCP server itself: capabilities, middleware and session
// hooks. Both transports share it.
//
// Middleware order matters. Rate limiting runs first so that a client hammering
// a blocked tool is throttled rather than generating unbounded refusals, and the
// read-only gate runs before any handler so a refused call never reaches Nomad.
//
// Rate limiting applies to the HTTP transport only. On stdio there is exactly
// one client, it is local, and the user started it themselves — the limiter's
// stated job of stopping one session from starving the others has no meaning
// there. It was also unconfigurable in that mode: MCP_RATE_LIMIT_GLOBAL and
// MCP_RATE_LIMIT_SESSION are scoped to the HTTP subcommands, so a stdio user
// hitting the limit had no flag to raise it. The e2e suite found this by doing
// what a troubleshooting session does — a dozen tool calls in a couple of
// seconds — and being refused partway through.
func NewServer(cfg *config.Config, logger *log.Logger, opts ...server.ServerOption) (*server.MCPServer, error) {
	provider, err := client.New(cfg, logger)
	if err != nil {
		return nil, err
	}

	gate := client.NewGate(cfg.ReadOnly, logger)

	hooks := &server.Hooks{}
	provider.RegisterHooks(hooks)

	defaultOpts := []server.ServerOption{
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithPromptCapabilities(true),
		server.WithRecovery(),
	}

	if cfg.IsHTTPMode() {
		limiter := client.NewRateLimiter(client.ParseRateLimits(cfg, logger), logger)
		limiter.RegisterHooks(hooks)
		defaultOpts = append(defaultOpts, server.WithToolHandlerMiddleware(limiter.Middleware()))
	}

	// Hooks are registered after the limiter has had its chance to add its own.
	defaultOpts = append(defaultOpts,
		server.WithHooks(hooks),
		server.WithToolHandlerMiddleware(gate.Middleware()),
	)
	opts = append(defaultOpts, opts...)

	s := server.NewMCPServer("nomad-mcp-server", version.Version, opts...)
	catalog := tools.InitTools(s, provider, gate)

	// Resources reuse the registered tool handlers rather than projecting Nomad
	// a second time, so an @-mentioned job and a read_job call return the same
	// bytes. Prompts are static text and take nothing but the provider.
	resources.New(provider, catalog).Register(s)
	prompts.New(provider).Register(s)

	if cfg.ReadOnly {
		logger.WithField("mutating_tools", len(gate.MutatingTools())).
			Info("read-only mode: mutating tools will be refused")
	}

	return s, nil
}

func runStdioServer(cfg *config.Config, logger *log.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logStartup(cfg, logger)

	mcpServer, err := NewServer(cfg, logger)
	if err != nil {
		return err
	}
	stdioServer := server.NewStdioServer(mcpServer)
	stdioServer.SetErrorLogger(stdlog.New(logger.Writer(), "stdioserver ", 0))

	errC := make(chan error, 1)
	go func() {
		errC <- stdioServer.Listen(ctx, os.Stdin, os.Stdout)
	}()

	// Announce on stderr, not stdout: stdout is the JSON-RPC channel and any
	// stray byte on it corrupts the protocol stream.
	fmt.Fprintf(os.Stderr, "Nomad MCP Server running on stdio (read-only: %t)\n", cfg.ReadOnly)

	select {
	case <-ctx.Done():
		logger.Info("shutting down stdio server")
		return nil
	case err := <-errC:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("stdio server error: %w", err)
		}
	}
	return nil
}

func runHTTPServer(cfg *config.Config, logger *log.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logStartup(cfg, logger)

	endpointPath := path.Join("/", cfg.MCPEndpoint)
	mcpServer, err := NewServer(cfg, logger)
	if err != nil {
		return err
	}

	streamable := server.NewStreamableHTTPServer(mcpServer,
		server.WithEndpointPath(endpointPath),
		server.WithStreamableHTTPLogger(utils.SlogFromLogrus(logger)),
	)

	// Wrapped outermost-last: logging sees every request, then per-request
	// Nomad settings are lifted out of headers, then the origin is validated
	// before anything reaches the MCP handler.
	cors := client.LoadCORSConfig(cfg)
	logCORS(cors, logger)

	handler := client.NewSecurityHandler(streamable, cors, logger)
	handler = client.NomadContextMiddleware(logger)(handler)
	handler = client.LoggingMiddleware(logger)(handler)

	mux := http.NewServeMux()
	mux.Handle(endpointPath, handler)
	mux.Handle(endpointPath+"/", handler)
	mux.HandleFunc("/health", healthHandler(cfg, endpointPath, logger))

	addr := fmt.Sprintf("%s:%s", cfg.TransportHost, cfg.TransportPort)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      30 * time.Second,
		// Long, because MCP clients hold the connection open between calls.
		IdleTimeout: 60 * time.Minute,
	}

	useTLS := cfg.MCPTLSCertFile != "" && cfg.MCPTLSKeyFile != ""
	if !useTLS && !isLocalhost(cfg.TransportHost) {
		return fmt.Errorf(
			"TLS is required when binding to a non-localhost address (%s). Set %s and %s",
			cfg.TransportHost, config.EnvMCPTLSCertFile, config.EnvMCPTLSKeyFile)
	}

	errC := make(chan error, 1)
	go func() {
		logger.WithFields(log.Fields{
			"addr":     addr,
			"endpoint": endpointPath,
			"tls":      useTLS,
		}).Info("starting StreamableHTTP server")

		if useTLS {
			errC <- httpServer.ListenAndServeTLS(cfg.MCPTLSCertFile, cfg.MCPTLSKeyFile)
			return
		}
		logger.Warn("TLS is disabled on the StreamableHTTP server; not recommended outside local development")
		errC <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down StreamableHTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errC:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("StreamableHTTP server error: %w", err)
		}
	}
	return nil
}

// healthHandler reports liveness plus the two facts an operator most often
// wants to confirm from outside: where the MCP endpoint is, and whether writes
// are enabled.
func healthHandler(cfg *config.Config, endpointPath string, logger *log.Logger) http.HandlerFunc {
	body := fmt.Sprintf(
		`{"status":"ok","service":"nomad-mcp-server","version":%q,"transport":"streamable-http","endpoint":%q,"read_only":%t}`,
		version.GetHumanVersion(), endpointPath, cfg.ReadOnly)

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(body)); err != nil {
			logger.WithError(err).Error("failed to write health check response")
		}
	}
}

// logCORS makes the effective origin policy visible at startup, since a
// misconfigured one shows up later as an opaque 403 in a browser client.
func logCORS(cors client.CORSConfig, logger *log.Logger) {
	logger.WithField("cors_mode", cors.Mode).Info("CORS policy loaded")

	switch {
	case len(cors.AllowedOrigins) > 0:
		logger.Infof("allowed origins: %s", strings.Join(cors.AllowedOrigins, ", "))
	case cors.Mode == "strict":
		logger.Warn("no allowed origins configured in strict mode: all cross-origin requests will be rejected")
	case cors.Mode == "development":
		logger.Info("development mode: localhost origins are allowed automatically")
	case cors.Mode == "disabled":
		logger.Warn("CORS validation is disabled; not recommended outside local development")
	}
}

// logStartup records the effective configuration. It never logs the token.
func logStartup(cfg *config.Config, logger *log.Logger) {
	fields := log.Fields{
		"version":              version.GetHumanVersion(),
		"nomad_addr":           cfg.NomadAddr,
		"nomad_namespace":      cfg.NomadNamespace,
		"nomad_token_set":      cfg.NomadToken != "",
		"read_only":            cfg.ReadOnly,
		"allow_variable_reads": cfg.AllowVariableReads,
		"max_log_bytes":        cfg.MaxLogBytes,
		"transport_mode":       cfg.TransportMode,
	}
	if len(cfg.AllowedNamespaces) > 0 {
		fields["allowed_namespaces"] = strings.Join(cfg.AllowedNamespaces, ",")
	}
	logger.WithFields(fields).Info("nomad-mcp-server starting")

	if !cfg.ReadOnly {
		logger.Warn("read-only mode is OFF: mutating tools are enabled and can change this cluster")
	}
	if cfg.AllowVariableReads {
		logger.Warn("variable reads are ON: read_variable will return Nomad Variable values, which commonly hold secrets")
	}
	if cfg.NomadSkipVerify {
		logger.Warn("TLS verification of the Nomad server is disabled")
	}
}

// isLocalhost reports whether a bind address is loopback, in which case serving
// plaintext HTTP is acceptable.
func isLocalhost(host string) bool {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
