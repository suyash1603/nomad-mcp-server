// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package config owns every knob the server has.
//
// There is exactly one list of settings — the `settings` table below — and
// everything else is derived from it: the CLI flags, the viper bindings, the
// environment variable names, and the defaults. Adding a knob means adding one
// row, not touching four files.
//
// Precedence is flag > environment variable > default, which is what viper does
// natively once a key is bound with both BindPFlag and BindEnv: a flag only wins
// when it was actually set on the command line, otherwise the lookup falls
// through to the environment and then to the default.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Environment variable names. The NOMAD_* names are the ones the `nomad` CLI
// itself honors, so a shell that can already run `nomad status` can run this
// server with no extra configuration.
const (
	EnvNomadAddr          = "NOMAD_ADDR"
	EnvNomadToken         = "NOMAD_TOKEN"
	EnvNomadRegion        = "NOMAD_REGION"
	EnvNomadNamespace     = "NOMAD_NAMESPACE"
	EnvNomadCACert        = "NOMAD_CACERT"
	EnvNomadCAPath        = "NOMAD_CAPATH"
	EnvNomadClientCert    = "NOMAD_CLIENT_CERT"
	EnvNomadClientKey     = "NOMAD_CLIENT_KEY"
	EnvNomadTLSServerName = "NOMAD_TLS_SERVER_NAME"
	EnvNomadSkipVerify    = "NOMAD_SKIP_VERIFY"
)

// Transport and server variables, named to match vault-mcp-server exactly.
const (
	EnvTransportMode     = "TRANSPORT_MODE"
	EnvTransportHost     = "TRANSPORT_HOST"
	EnvTransportPort     = "TRANSPORT_PORT"
	EnvMCPEndpoint       = "MCP_ENDPOINT"
	EnvMCPAllowedOrigins = "MCP_ALLOWED_ORIGINS"
	EnvMCPCORSMode       = "MCP_CORS_MODE"
	EnvMCPTLSCertFile    = "MCP_TLS_CERT_FILE"
	EnvMCPTLSKeyFile     = "MCP_TLS_KEY_FILE"
	EnvMCPRateLimitGlob  = "MCP_RATE_LIMIT_GLOBAL"
	EnvMCPRateLimitSess  = "MCP_RATE_LIMIT_SESSION"
)

// Variables specific to this server.
const (
	EnvReadOnly           = "NOMAD_MCP_READ_ONLY"
	EnvAllowedNamespaces  = "NOMAD_MCP_ALLOWED_NAMESPACES"
	EnvAllowVariableReads = "NOMAD_MCP_ALLOW_VARIABLE_READS"
	EnvMaxLogBytes        = "NOMAD_MCP_MAX_LOG_BYTES"
	EnvAllowDestructive   = "NOMAD_MCP_ALLOW_DESTRUCTIVE"
	EnvEnterprise         = "NOMAD_MCP_ENTERPRISE"
	EnvLogFile            = "NOMAD_MCP_LOG_FILE"
	EnvLogLevel           = "NOMAD_MCP_LOG_LEVEL"
)

// Defaults. Exported because the README, the tests, and the error messages all
// need to agree on them.
const (
	DefaultNomadAddr      = "http://127.0.0.1:4646"
	DefaultNomadNamespace = "default"
	DefaultTransportMode  = "stdio"
	DefaultTransportHost  = "127.0.0.1"
	DefaultTransportPort  = "8080"
	DefaultMCPEndpoint    = "/mcp"
	DefaultCORSMode       = "strict"
	DefaultRateLimitGlob  = "10:20"
	DefaultRateLimitSess  = "5:10"
	DefaultLogLevel       = "info"

	// DefaultReadOnly is true and that is deliberate. See docs/SECURITY.md.
	DefaultReadOnly = true

	// DefaultMaxLogBytes caps log and file reads so a chatty task cannot
	// exhaust the model's context window.
	DefaultMaxLogBytes = 65536

	// DefaultAllowDestructive is true, which means turning writes on turns all
	// of them on. The alternative — a second flag to unlock node purges and
	// namespace deletions — was rejected as the default because writes are
	// already off unless the operator deliberately enabled them, and a second
	// gate they did not know about would look like a broken tool. Operators who
	// do want the middle tier set it to false.
	DefaultAllowDestructive = true

	// DefaultEnterprise is "auto": probe the cluster once at startup and offer
	// the Enterprise-only tools unless the cluster is known to be Community
	// Edition. Probing is best-effort, and an unreachable cluster resolves to
	// offering them, so a server started before its Nomad does not come up
	// missing half its catalog.
	DefaultEnterprise = "auto"
)

// scope says which command a setting's flag is registered on.
type scope int

const (
	// scopeRoot registers the flag as a persistent flag on the root command,
	// so it applies to every subcommand.
	scopeRoot scope = iota
	// scopeHTTP registers the flag only on the HTTP transport commands.
	scopeHTTP
)

// kind is the flag's Go type.
type kind int

const (
	kindString kind = iota
	kindBool
	kindInt
)

// setting is one configuration knob: one flag, one environment variable, one
// default. `key` doubles as the CLI flag name and the viper key.
type setting struct {
	key   string
	env   string
	kind  kind
	def   any
	scope scope
	usage string
}

// settings is the single source of truth for the server's configuration.
//
// NOMAD_TOKEN is deliberately absent: it is environment-only, with no
// corresponding flag. A token passed as a command-line argument is visible to
// every other process on the machine via `ps`, and lands in shell history. The
// `nomad` CLI makes the same choice.
var settings = []setting{
	// --- Nomad connection -------------------------------------------------
	{"nomad-addr", EnvNomadAddr, kindString, DefaultNomadAddr, scopeRoot,
		"Address of the Nomad HTTP API"},
	{"nomad-region", EnvNomadRegion, kindString, "", scopeRoot,
		"Nomad region to target (default: the agent's own region)"},
	{"nomad-namespace", EnvNomadNamespace, kindString, DefaultNomadNamespace, scopeRoot,
		"Default Nomad namespace for namespaced tools"},
	{"nomad-ca-cert", EnvNomadCACert, kindString, "", scopeRoot,
		"Path to a PEM-encoded CA certificate file used to verify the Nomad server"},
	{"nomad-ca-path", EnvNomadCAPath, kindString, "", scopeRoot,
		"Path to a directory of PEM-encoded CA certificate files"},
	{"nomad-client-cert", EnvNomadClientCert, kindString, "", scopeRoot,
		"Path to a PEM-encoded client certificate for mTLS"},
	{"nomad-client-key", EnvNomadClientKey, kindString, "", scopeRoot,
		"Path to a PEM-encoded client key for mTLS"},
	{"nomad-tls-server-name", EnvNomadTLSServerName, kindString, "", scopeRoot,
		"Server name to use as the SNI host when connecting over TLS"},
	{"nomad-skip-verify", EnvNomadSkipVerify, kindBool, false, scopeRoot,
		"Do not verify the Nomad server's TLS certificate (insecure)"},

	// --- Safety -----------------------------------------------------------
	{"read-only", EnvReadOnly, kindBool, DefaultReadOnly, scopeRoot,
		"Refuse every mutating tool. Safe by default; pass --read-only=false to allow writes"},
	{"allowed-namespaces", EnvAllowedNamespaces, kindString, "", scopeRoot,
		"Comma-separated allowlist of Nomad namespaces tools may touch (default: all)"},
	{"allow-variable-reads", EnvAllowVariableReads, kindBool, false, scopeRoot,
		"Allow read_variable to return Nomad Variable values. Variables hold secrets"},
	{"max-log-bytes", EnvMaxLogBytes, kindInt, DefaultMaxLogBytes, scopeRoot,
		"Maximum bytes returned by log and allocation file reads before truncation"},
	{"allow-destructive", EnvAllowDestructive, kindBool, DefaultAllowDestructive, scopeRoot,
		"Allow tools that discard state or interrupt running work. Set false for a writes-but-nothing-irreversible tier"},
	{"enterprise", EnvEnterprise, kindString, DefaultEnterprise, scopeRoot,
		"Offer the Nomad Enterprise-only tools: auto (probe the cluster), true (always), or false (never)"},

	// --- Logging ----------------------------------------------------------
	{"log-file", EnvLogFile, kindString, "", scopeRoot,
		"Path to a log file (default: stderr)"},
	{"log-level", EnvLogLevel, kindString, DefaultLogLevel, scopeRoot,
		"Log level: trace, debug, info, warn, error"},

	// --- Transport --------------------------------------------------------
	{"transport-mode", EnvTransportMode, kindString, DefaultTransportMode, scopeRoot,
		"Transport to serve when no subcommand is given: stdio or http"},
	{"transport-host", EnvTransportHost, kindString, DefaultTransportHost, scopeHTTP,
		"Host to bind the HTTP transport to"},
	{"transport-port", EnvTransportPort, kindString, DefaultTransportPort, scopeHTTP,
		"Port to bind the HTTP transport to"},
	{"mcp-endpoint", EnvMCPEndpoint, kindString, DefaultMCPEndpoint, scopeHTTP,
		"Path the streamable-HTTP endpoint is served on"},
	{"mcp-allowed-origins", EnvMCPAllowedOrigins, kindString, "", scopeHTTP,
		"Comma-separated list of origins allowed by CORS"},
	{"mcp-cors-mode", EnvMCPCORSMode, kindString, DefaultCORSMode, scopeHTTP,
		"CORS enforcement: strict, development, or disabled"},
	{"mcp-tls-cert-file", EnvMCPTLSCertFile, kindString, "", scopeHTTP,
		"Path to a TLS certificate for the HTTP transport"},
	{"mcp-tls-key-file", EnvMCPTLSKeyFile, kindString, "", scopeHTTP,
		"Path to a TLS key for the HTTP transport"},
	{"mcp-rate-limit-global", EnvMCPRateLimitGlob, kindString, DefaultRateLimitGlob, scopeHTTP,
		"Global rate limit as rps:burst"},
	{"mcp-rate-limit-session", EnvMCPRateLimitSess, kindString, DefaultRateLimitSess, scopeHTTP,
		"Per-session rate limit as rps:burst"},
}

// Config is the resolved configuration for one run of the server.
type Config struct {
	// Nomad connection.
	NomadAddr          string
	NomadToken         string
	NomadRegion        string
	NomadNamespace     string
	NomadCACert        string
	NomadCAPath        string
	NomadClientCert    string
	NomadClientKey     string
	NomadTLSServerName string
	NomadSkipVerify    bool

	// Safety.
	ReadOnly           bool
	AllowedNamespaces  []string
	AllowVariableReads bool
	MaxLogBytes        int64
	AllowDestructive   bool
	Enterprise         string

	// Logging.
	LogFile  string
	LogLevel string

	// Transport.
	TransportMode     string
	TransportHost     string
	TransportPort     string
	MCPEndpoint       string
	MCPAllowedOrigins []string
	MCPCORSMode       string
	MCPTLSCertFile    string
	MCPTLSKeyFile     string
	RateLimitGlobal   string
	RateLimitSession  string
}

// RegisterFlags declares every flag on the right command and binds it to viper.
//
// rootCmd receives the scopeRoot settings as persistent flags. Each command in
// httpCmds receives the scopeHTTP settings; there is more than one because the
// `http` alias must accept the same flags as `streamable-http`.
//
// Viper keys are global, so binding the same key from two sibling commands is
// safe: only the command that actually runs will have parsed its flags, and an
// unparsed flag reports Changed() == false and is therefore ignored.
func RegisterFlags(rootCmd *cobra.Command, httpCmds ...*cobra.Command) error {
	for _, s := range settings {
		switch s.scope {
		case scopeRoot:
			define(rootCmd.PersistentFlags(), s)
			if err := bind(rootCmd.PersistentFlags(), s); err != nil {
				return err
			}
		case scopeHTTP:
			for _, c := range httpCmds {
				define(c.Flags(), s)
			}
			// Bind from the first command only. Viper holds a pointer to the
			// *pflag.Flag, and the alias command's flag set is a duplicate of
			// the primary one; binding twice would leave whichever bound last
			// in charge, regardless of which command the user actually ran.
			if len(httpCmds) > 0 {
				if err := bind(httpCmds[0].Flags(), s); err != nil {
					return err
				}
			}
		}
	}

	// NOMAD_TOKEN has no flag, so bind it to the environment directly.
	return viper.BindEnv("nomad-token", EnvNomadToken)
}

// define adds a single flag to a flag set with the right type and default.
func define(fs *pflag.FlagSet, s setting) {
	switch s.kind {
	case kindString:
		fs.String(s.key, s.def.(string), usageWithEnv(s))
	case kindBool:
		fs.Bool(s.key, s.def.(bool), usageWithEnv(s))
	case kindInt:
		fs.Int(s.key, s.def.(int), usageWithEnv(s))
	}
}

// bind wires a flag and its environment variable to the same viper key, and
// records the default so that an unset flag with an unset env still resolves.
func bind(fs *pflag.FlagSet, s setting) error {
	viper.SetDefault(s.key, s.def)
	if err := viper.BindPFlag(s.key, fs.Lookup(s.key)); err != nil {
		return fmt.Errorf("binding flag --%s: %w", s.key, err)
	}
	if err := viper.BindEnv(s.key, s.env); err != nil {
		return fmt.Errorf("binding env %s: %w", s.env, err)
	}
	return nil
}

// usageWithEnv appends the environment variable name to a flag's help text, so
// that `--help` documents both ways of setting every knob.
func usageWithEnv(s setting) string {
	return fmt.Sprintf("%s [%s]", s.usage, s.env)
}

// Load resolves the configuration and validates it.
func Load() (*Config, error) {
	c := &Config{
		NomadAddr:          viper.GetString("nomad-addr"),
		NomadToken:         viper.GetString("nomad-token"),
		NomadRegion:        viper.GetString("nomad-region"),
		NomadNamespace:     viper.GetString("nomad-namespace"),
		NomadCACert:        viper.GetString("nomad-ca-cert"),
		NomadCAPath:        viper.GetString("nomad-ca-path"),
		NomadClientCert:    viper.GetString("nomad-client-cert"),
		NomadClientKey:     viper.GetString("nomad-client-key"),
		NomadTLSServerName: viper.GetString("nomad-tls-server-name"),
		NomadSkipVerify:    viper.GetBool("nomad-skip-verify"),

		ReadOnly:           viper.GetBool("read-only"),
		AllowedNamespaces:  splitList(viper.GetString("allowed-namespaces")),
		AllowVariableReads: viper.GetBool("allow-variable-reads"),
		MaxLogBytes:        int64(viper.GetInt("max-log-bytes")),
		AllowDestructive:   viper.GetBool("allow-destructive"),
		Enterprise:         strings.ToLower(strings.TrimSpace(viper.GetString("enterprise"))),

		LogFile:  viper.GetString("log-file"),
		LogLevel: viper.GetString("log-level"),

		TransportMode:     viper.GetString("transport-mode"),
		TransportHost:     viper.GetString("transport-host"),
		TransportPort:     viper.GetString("transport-port"),
		MCPEndpoint:       viper.GetString("mcp-endpoint"),
		MCPAllowedOrigins: splitList(viper.GetString("mcp-allowed-origins")),
		MCPCORSMode:       viper.GetString("mcp-cors-mode"),
		MCPTLSCertFile:    viper.GetString("mcp-tls-cert-file"),
		MCPTLSKeyFile:     viper.GetString("mcp-tls-key-file"),
		RateLimitGlobal:   viper.GetString("mcp-rate-limit-global"),
		RateLimitSession:  viper.GetString("mcp-rate-limit-session"),
	}

	if c.NomadNamespace == "" {
		c.NomadNamespace = DefaultNomadNamespace
	}
	if c.Enterprise == "" {
		c.Enterprise = DefaultEnterprise
	}

	// Parity with vault-mcp-server: setting any of the HTTP transport
	// variables selects HTTP mode even when TRANSPORT_MODE itself is unset.
	// Without this, `TRANSPORT_PORT=9000 nomad-mcp-server` would silently
	// serve stdio and ignore the port.
	if !c.IsHTTPMode() && httpModeImpliedByEnv() {
		c.TransportMode = "http"
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate rejects a configuration that would fail later in a confusing way.
// Everything here is checked at startup so the operator hears about it once,
// rather than as a tool error on the model's first call.
func (c *Config) Validate() error {
	switch c.MCPCORSMode {
	case "strict", "development", "disabled":
	default:
		return fmt.Errorf("invalid %s %q: must be strict, development, or disabled",
			EnvMCPCORSMode, c.MCPCORSMode)
	}

	switch c.TransportMode {
	case "stdio", "http", "streamable-http":
	default:
		return fmt.Errorf("invalid %s %q: must be stdio or http",
			EnvTransportMode, c.TransportMode)
	}

	switch c.LogLevel {
	case "trace", "debug", "info", "warn", "warning", "error", "fatal", "panic":
	default:
		return fmt.Errorf("invalid %s %q: must be one of trace, debug, info, warn, error",
			EnvLogLevel, c.LogLevel)
	}

	switch c.Enterprise {
	case "auto", "true", "false":
	default:
		return fmt.Errorf("invalid %s %q: must be auto, true, or false",
			EnvEnterprise, c.Enterprise)
	}

	if c.MaxLogBytes <= 0 {
		return fmt.Errorf("invalid %s %d: must be greater than zero",
			EnvMaxLogBytes, c.MaxLogBytes)
	}

	if err := validateRateLimit(EnvMCPRateLimitGlob, c.RateLimitGlobal); err != nil {
		return err
	}
	if err := validateRateLimit(EnvMCPRateLimitSess, c.RateLimitSession); err != nil {
		return err
	}

	// A cert without its key (or vice versa) is always a mistake, and the
	// failure mode otherwise is a TLS handshake error much later.
	if (c.MCPTLSCertFile == "") != (c.MCPTLSKeyFile == "") {
		return fmt.Errorf("%s and %s must be set together",
			EnvMCPTLSCertFile, EnvMCPTLSKeyFile)
	}
	if (c.NomadClientCert == "") != (c.NomadClientKey == "") {
		return fmt.Errorf("%s and %s must be set together",
			EnvNomadClientCert, EnvNomadClientKey)
	}

	return nil
}

// validateRateLimit checks the "rps:burst" form shared by both rate limits.
func validateRateLimit(name, v string) error {
	rps, burst, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("invalid %s %q: expected rps:burst, for example %q",
			name, v, DefaultRateLimitGlob)
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(rps), 64)
	if err != nil || f <= 0 {
		return fmt.Errorf("invalid %s %q: rps must be a positive number", name, v)
	}
	b, err := strconv.Atoi(strings.TrimSpace(burst))
	if err != nil || b <= 0 {
		return fmt.Errorf("invalid %s %q: burst must be a positive integer", name, v)
	}
	return nil
}

// httpModeImpliedByEnv reports whether an HTTP-only transport variable is set
// in the environment.
func httpModeImpliedByEnv() bool {
	for _, e := range []string{EnvTransportHost, EnvTransportPort, EnvMCPEndpoint} {
		if v, ok := os.LookupEnv(e); ok && v != "" {
			return true
		}
	}
	return false
}

// IsHTTPMode reports whether the resolved transport mode means HTTP.
func (c *Config) IsHTTPMode() bool {
	return c.TransportMode == "http" || c.TransportMode == "streamable-http"
}

// NamespaceAllowed reports whether a namespace may be touched. An empty
// allowlist means every namespace is allowed, which is the default.
func (c *Config) NamespaceAllowed(ns string) bool {
	if len(c.AllowedNamespaces) == 0 {
		return true
	}
	for _, a := range c.AllowedNamespaces {
		if a == ns {
			return true
		}
	}
	return false
}

// splitList parses a comma-separated list, dropping empty entries.
//
// This is a plain string rather than a pflag StringSlice on purpose: viper's
// GetStringSlice does not split an environment variable on commas, so binding a
// slice flag to an env var silently yields a one-element slice containing the
// whole string. Splitting here keeps the flag and the env var behaving alike.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// EnterpriseAlways reports whether the Enterprise-only tools are offered
// unconditionally, without probing the cluster.
func (c *Config) EnterpriseAlways() bool { return c.Enterprise == "true" }

// EnterpriseNever reports whether the Enterprise-only tools are suppressed
// unconditionally. Useful on a Community Edition cluster whose operator would
// rather the model never saw a tool it cannot use.
func (c *Config) EnterpriseNever() bool { return c.Enterprise == "false" }

// EnterpriseAuto reports whether the decision is left to a cluster probe.
func (c *Config) EnterpriseAuto() bool { return c.Enterprise == "auto" || c.Enterprise == "" }
