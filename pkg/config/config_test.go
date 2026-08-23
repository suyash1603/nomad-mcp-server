// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

// load builds a fresh command tree, parses args, and resolves the config the
// same way a real invocation does.
//
// These tests cannot run in parallel: viper keeps its bindings in a package
// global, so each case resets it and rebuilds the commands from scratch.
func load(t *testing.T, args ...string) (*Config, error) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)

	root := &cobra.Command{Use: "nomad-mcp-server"}
	httpCmd := &cobra.Command{Use: "streamable-http"}
	aliasCmd := &cobra.Command{Use: "http"}
	root.AddCommand(httpCmd, aliasCmd)

	require.NoError(t, RegisterFlags(root, httpCmd, aliasCmd))

	// Root-scoped flags are persistent, so parsing them on the HTTP command
	// exercises both scopes at once — which is how a real run works. A parse
	// error is returned rather than asserted on, so that tests can assert
	// against a rejected flag.
	if err := httpCmd.ParseFlags(args); err != nil {
		return nil, err
	}

	return Load()
}

func TestDefaults(t *testing.T) {
	cfg, err := load(t)
	require.NoError(t, err)

	require.Equal(t, DefaultNomadAddr, cfg.NomadAddr)
	require.Equal(t, DefaultNomadNamespace, cfg.NomadNamespace)
	require.Equal(t, DefaultTransportMode, cfg.TransportMode)
	require.Equal(t, DefaultTransportPort, cfg.TransportPort)
	require.Equal(t, DefaultMCPEndpoint, cfg.MCPEndpoint)
	require.Equal(t, DefaultCORSMode, cfg.MCPCORSMode)
	require.Equal(t, int64(DefaultMaxLogBytes), cfg.MaxLogBytes)
	require.Empty(t, cfg.AllowedNamespaces)
	require.Empty(t, cfg.NomadToken)
}

// TestReadOnlyDefaultsOn guards the one setting whose default is a security
// property rather than a preference. See docs/SECURITY.md.
func TestReadOnlyDefaultsOn(t *testing.T) {
	cfg, err := load(t)
	require.NoError(t, err)
	require.True(t, cfg.ReadOnly, "read-only must default to true")

	cfg, err = load(t, "--read-only=false")
	require.NoError(t, err)
	require.False(t, cfg.ReadOnly)
}

func TestVariableReadsDefaultOff(t *testing.T) {
	cfg, err := load(t)
	require.NoError(t, err)
	require.False(t, cfg.AllowVariableReads, "variable reads must default to off")
}

func TestEnvIsHonored(t *testing.T) {
	t.Setenv(EnvNomadAddr, "http://nomad.internal:4646")
	t.Setenv(EnvNomadNamespace, "prod")
	t.Setenv(EnvNomadToken, "not-a-real-token")
	t.Setenv(EnvReadOnly, "false")
	t.Setenv(EnvMaxLogBytes, "1024")

	cfg, err := load(t)
	require.NoError(t, err)

	require.Equal(t, "http://nomad.internal:4646", cfg.NomadAddr)
	require.Equal(t, "prod", cfg.NomadNamespace)
	require.Equal(t, "not-a-real-token", cfg.NomadToken)
	require.False(t, cfg.ReadOnly)
	require.Equal(t, int64(1024), cfg.MaxLogBytes)
}

// TestFlagBeatsEnv pins the precedence rule the whole config layer rests on:
// flag beats environment variable beats default.
func TestFlagBeatsEnv(t *testing.T) {
	t.Setenv(EnvNomadAddr, "http://from-env:4646")
	t.Setenv(EnvNomadNamespace, "from-env")
	t.Setenv(EnvReadOnly, "true")
	t.Setenv(EnvMaxLogBytes, "1024")
	t.Setenv(EnvTransportPort, "1111")

	cfg, err := load(t,
		"--nomad-addr", "http://from-flag:4646",
		"--nomad-namespace", "from-flag",
		"--read-only=false",
		"--max-log-bytes", "4096",
		"--transport-port", "2222",
	)
	require.NoError(t, err)

	require.Equal(t, "http://from-flag:4646", cfg.NomadAddr)
	require.Equal(t, "from-flag", cfg.NomadNamespace)
	require.False(t, cfg.ReadOnly, "--read-only=false must beat NOMAD_MCP_READ_ONLY=true")
	require.Equal(t, int64(4096), cfg.MaxLogBytes)
	require.Equal(t, "2222", cfg.TransportPort)
}

// TestEnvBeatsDefault covers the middle rung: no flag given, so the env wins
// over the built-in default rather than the flag's zero value winning.
func TestEnvBeatsDefault(t *testing.T) {
	t.Setenv(EnvMCPCORSMode, "development")
	cfg, err := load(t)
	require.NoError(t, err)
	require.Equal(t, "development", cfg.MCPCORSMode)
}

// TestNoTokenFlag documents a deliberate omission: a token on the command line
// is visible in `ps` and in shell history, so NOMAD_TOKEN is env-only.
func TestNoTokenFlag(t *testing.T) {
	_, err := load(t, "--nomad-token", "leaked")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown flag")
}

func TestAllowedNamespacesParsing(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty means all", "", nil},
		{"single", "prod", []string{"prod"}},
		{"multiple", "prod,staging", []string{"prod", "staging"}},
		{"whitespace trimmed", " prod , staging ", []string{"prod", "staging"}},
		{"empty entries dropped", "prod,,staging,", []string{"prod", "staging"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := load(t, "--allowed-namespaces", tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.AllowedNamespaces)
		})
	}
}

// TestAllowedNamespacesFromEnvSplitsOnComma is the reason AllowedNamespaces is
// a string flag rather than a StringSlice: viper does not split an environment
// variable on commas, which would silently yield one namespace named
// "prod,staging" and reject both.
func TestAllowedNamespacesFromEnvSplitsOnComma(t *testing.T) {
	t.Setenv(EnvAllowedNamespaces, "prod,staging")
	cfg, err := load(t)
	require.NoError(t, err)
	require.Equal(t, []string{"prod", "staging"}, cfg.AllowedNamespaces)
}

func TestNamespaceAllowed(t *testing.T) {
	open := &Config{}
	require.True(t, open.NamespaceAllowed("anything"),
		"an empty allowlist must permit every namespace")

	scoped := &Config{AllowedNamespaces: []string{"prod", "staging"}}
	require.True(t, scoped.NamespaceAllowed("prod"))
	require.True(t, scoped.NamespaceAllowed("staging"))
	require.False(t, scoped.NamespaceAllowed("default"))
	require.False(t, scoped.NamespaceAllowed(""))
	require.False(t, scoped.NamespaceAllowed("PROD"), "matching must be exact")
}

func TestHTTPEnvImpliesHTTPMode(t *testing.T) {
	for _, env := range []string{EnvTransportHost, EnvTransportPort, EnvMCPEndpoint} {
		t.Run(env, func(t *testing.T) {
			switch env {
			case EnvTransportHost:
				t.Setenv(env, "127.0.0.1")
			case EnvTransportPort:
				t.Setenv(env, "9000")
			case EnvMCPEndpoint:
				t.Setenv(env, "/mcp")
			}
			cfg, err := load(t)
			require.NoError(t, err)
			require.True(t, cfg.IsHTTPMode(),
				"%s should select HTTP mode even with TRANSPORT_MODE unset", env)
		})
	}
}

func TestStdioIsDefaultTransport(t *testing.T) {
	cfg, err := load(t)
	require.NoError(t, err)
	require.False(t, cfg.IsHTTPMode())
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"bad cors mode", []string{"--mcp-cors-mode", "loose"}, "must be strict, development, or disabled"},
		{"bad transport mode", []string{"--transport-mode", "carrier-pigeon"}, "must be stdio or http"},
		{"bad log level", []string{"--log-level", "chatty"}, "must be one of trace, debug, info"},
		{"zero max log bytes", []string{"--max-log-bytes", "0"}, "must be greater than zero"},
		{"negative max log bytes", []string{"--max-log-bytes", "-1"}, "must be greater than zero"},
		{"rate limit missing colon", []string{"--mcp-rate-limit-global", "10"}, "expected rps:burst"},
		{"rate limit bad rps", []string{"--mcp-rate-limit-global", "x:20"}, "rps must be a positive number"},
		{"rate limit zero rps", []string{"--mcp-rate-limit-global", "0:20"}, "rps must be a positive number"},
		{"rate limit bad burst", []string{"--mcp-rate-limit-session", "10:x"}, "burst must be a positive integer"},
		{"mcp cert without key", []string{"--mcp-tls-cert-file", "/tmp/c.pem"}, "must be set together"},
		{"mcp key without cert", []string{"--mcp-tls-key-file", "/tmp/k.pem"}, "must be set together"},
		{"nomad cert without key", []string{"--nomad-client-cert", "/tmp/c.pem"}, "must be set together"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.args...)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestValidationErrorsNameTheEnvVar keeps error messages actionable: an
// operator who set a value via the environment needs to be told which
// environment variable to go fix, not which internal key was rejected.
func TestValidationErrorsNameTheEnvVar(t *testing.T) {
	_, err := load(t, "--mcp-cors-mode", "loose")
	require.Error(t, err)
	require.Contains(t, err.Error(), EnvMCPCORSMode)
}

func TestValidAcceptedValues(t *testing.T) {
	for _, mode := range []string{"strict", "development", "disabled"} {
		_, err := load(t, "--mcp-cors-mode", mode)
		require.NoError(t, err, "cors mode %q should be valid", mode)
	}
	for _, mode := range []string{"stdio", "http", "streamable-http"} {
		_, err := load(t, "--transport-mode", mode)
		require.NoError(t, err, "transport mode %q should be valid", mode)
	}
	for _, lvl := range []string{"trace", "debug", "info", "warn", "error"} {
		_, err := load(t, "--log-level", lvl)
		require.NoError(t, err, "log level %q should be valid", lvl)
	}
}

// TestEmptyNamespaceFallsBackToDefault covers NOMAD_NAMESPACE="" in an
// exported shell, which would otherwise leave the namespace blank and send
// unnamespaced requests.
func TestEmptyNamespaceFallsBackToDefault(t *testing.T) {
	t.Setenv(EnvNomadNamespace, "")
	cfg, err := load(t)
	require.NoError(t, err)
	require.Equal(t, DefaultNomadNamespace, cfg.NomadNamespace)
}

// TestEverySettingHasAFlagAndEnv is a guard on the settings table itself: the
// requirement is that every knob is reachable both ways.
func TestEverySettingHasAFlagAndEnv(t *testing.T) {
	seenKeys := map[string]bool{}
	seenEnvs := map[string]bool{}

	for _, s := range settings {
		require.NotEmpty(t, s.key, "every setting needs a flag name")
		require.NotEmpty(t, s.env, "setting %q needs an environment variable", s.key)
		require.NotEmpty(t, s.usage, "setting %q needs usage text", s.key)
		require.False(t, seenKeys[s.key], "duplicate flag name %q", s.key)
		require.False(t, seenEnvs[s.env], "duplicate env var %q", s.env)
		seenKeys[s.key] = true
		seenEnvs[s.env] = true
	}
}

// TestHelpDocumentsEnvVars keeps `--help` self-describing, so an operator does
// not have to cross-reference the README to find the environment variable.
func TestHelpDocumentsEnvVars(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	root := &cobra.Command{Use: "nomad-mcp-server"}
	httpCmd := &cobra.Command{Use: "streamable-http"}
	root.AddCommand(httpCmd)
	require.NoError(t, RegisterFlags(root, httpCmd))

	usage := root.PersistentFlags().FlagUsages() + httpCmd.Flags().FlagUsages()
	for _, s := range settings {
		require.Contains(t, usage, s.env,
			"--help should mention %s for --%s", s.env, s.key)
	}
}

// --- the new safety and edition knobs ---------------------------------------

func TestEnterpriseModeAcceptsItsThreeValues(t *testing.T) {
	for _, v := range []string{"auto", "true", "false"} {
		c := &Config{
			Enterprise:       v,
			MaxLogBytes:      DefaultMaxLogBytes,
			MCPCORSMode:      DefaultCORSMode,
			TransportMode:    DefaultTransportMode,
			LogLevel:         DefaultLogLevel,
			RateLimitGlobal:  DefaultRateLimitGlob,
			RateLimitSession: DefaultRateLimitSess,
		}
		require.NoError(t, c.Validate(), "%q should be a valid NOMAD_MCP_ENTERPRISE", v)
	}
}

func TestEnterpriseModeRejectsAnythingElse(t *testing.T) {
	c := &Config{
		Enterprise:       "yes",
		MaxLogBytes:      DefaultMaxLogBytes,
		MCPCORSMode:      DefaultCORSMode,
		TransportMode:    DefaultTransportMode,
		LogLevel:         DefaultLogLevel,
		RateLimitGlobal:  DefaultRateLimitGlob,
		RateLimitSession: DefaultRateLimitSess,
	}

	err := c.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), EnvEnterprise,
		"the error must name the variable the operator has to fix")
}

func TestEnterpriseModeHelpers(t *testing.T) {
	require.True(t, (&Config{Enterprise: "auto"}).EnterpriseAuto())
	require.True(t, (&Config{Enterprise: ""}).EnterpriseAuto(),
		"an unset value must behave as auto rather than as never")
	require.True(t, (&Config{Enterprise: "true"}).EnterpriseAlways())
	require.True(t, (&Config{Enterprise: "false"}).EnterpriseNever())

	// The three are mutually exclusive; a mode that answered yes to two would
	// make the decision in includeEnterpriseTools order-dependent.
	for _, v := range []string{"auto", "true", "false"} {
		c := &Config{Enterprise: v}
		n := 0
		for _, on := range []bool{c.EnterpriseAuto(), c.EnterpriseAlways(), c.EnterpriseNever()} {
			if on {
				n++
			}
		}
		require.Equal(t, 1, n, "%q should match exactly one mode", v)
	}
}

// Enabling writes must still enable all of them. A second gate defaulting to
// closed would silently break every operator already running with writes on.
func TestDestructiveIsAllowedByDefault(t *testing.T) {
	require.True(t, DefaultAllowDestructive,
		"turning off read-only must not leave destructive tools quietly blocked")
}
