// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"os"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"

	log "github.com/sirupsen/logrus"
)

func init() {
	rootCmd.SetVersionTemplate("{{.Short}}\n{{.Version}}\n")

	rootCmd.AddCommand(stdioCmd, streamableHTTPCmd, httpCmdAlias)

	// One call declares every flag on the right command and binds each to its
	// environment variable. See pkg/config for the settings table.
	if err := config.RegisterFlags(rootCmd, streamableHTTPCmd, httpCmdAlias); err != nil {
		fmt.Fprintln(os.Stderr, "Error: failed to register flags:", err)
		os.Exit(1)
	}
}

// initLogger builds the logger.
//
// The default sink is stderr, never stdout: in stdio transport, stdout carries
// the JSON-RPC stream and a single log line written there corrupts the protocol.
func initLogger(cfg *config.Config) (*log.Logger, error) {
	logger := log.New()
	logger.SetOutput(os.Stderr)

	level, err := log.ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", config.EnvLogLevel, cfg.LogLevel, err)
	}
	logger.SetLevel(level)

	if cfg.LogFile == "" {
		return logger, nil
	}

	file, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %q: %w", cfg.LogFile, err)
	}
	logger.SetOutput(file)
	return logger, nil
}
