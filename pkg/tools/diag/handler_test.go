// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package diag

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// These drive the handler with a fake hcdiag on PATH. A stub of the real thing
// is what makes the whole path testable — argument construction, bundle
// discovery, exit-code handling and the timeout — none of which a unit test of
// the helpers reaches.

func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		// The fakes are shell scripts. Nothing here is platform-specific in
		// the code under test, so skipping the harness is better than
		// maintaining two of them.
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeHCDiag writes an executable script and returns its path. The script
// records the argv it was given so the test can assert on what was built.
func fakeHCDiag(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "hcdiag")

	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"" + filepath.Join(dir, "argv") + "\"\n" + body
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// argvOf returns the arguments the fake was last invoked with.
func argvOf(t *testing.T, binary string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(filepath.Dir(binary), "argv"))
	require.NoError(t, err, "the fake hcdiag was never invoked")
	return strings.TrimSpace(string(data))
}

func providerFor(t *testing.T, mutate func(*config.Config)) *client.Provider {
	t.Helper()

	logger := log.New()
	logger.SetOutput(io.Discard)

	cfg := &config.Config{
		NomadAddr:     "http://127.0.0.1:4646",
		NomadToken:    "s3cret-token-value",
		MaxLogBytes:   config.DefaultMaxLogBytes,
		EnableHCDiag:  true,
		HCDiagPath:    "hcdiag",
		HCDiagTimeout: config.DefaultHCDiagTimeoutForTests(),
	}
	mutate(cfg)

	p, err := client.New(cfg, logger)
	require.NoError(t, err)
	return p
}

func call(t *testing.T, p *client.Provider, args map[string]any) (map[string]any, bool, string) {
	t.Helper()

	var req mcp.CallToolRequest
	req.Params.Name = "collect_hcdiag"
	req.Params.Arguments = args

	res, err := CollectHCDiag(p).Handler(context.Background(), req)
	require.NoError(t, err, "tools report failure through the result, not a Go error")
	require.NotNil(t, res)

	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}

	var out map[string]any
	_ = json.Unmarshal([]byte(text.String()), &out)
	return out, res.IsError, text.String()
}

func TestDisabledByDefault(t *testing.T) {
	p := providerFor(t, func(c *config.Config) { c.EnableHCDiag = false })

	_, isErr, text := call(t, p, nil)
	require.True(t, isErr)
	require.Contains(t, text, "NOMAD_MCP_ENABLE_HCDIAG=true")
}

func TestMissingBinaryIsExplained(t *testing.T) {
	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = filepath.Join(t.TempDir(), "definitely-not-here")
	})

	_, isErr, text := call(t, p, nil)
	require.True(t, isErr)
	require.Contains(t, text, "was not found")
	require.Contains(t, text, "releases.hashicorp.com/hcdiag")
}

func TestNomadDiagnosticsAreRequestedWithTheGivenWindow(t *testing.T) {
	dest := t.TempDir()
	binary := fakeHCDiag(t, "exit 0\n")

	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = dest
	})

	call(t, p, map[string]any{"since": "4h"})

	argv := argvOf(t, binary)
	require.Contains(t, argv, "-nomad")
	require.Contains(t, argv, "-since 4h")
	require.Contains(t, argv, "-dest "+dest)
	require.NotContains(t, argv, "-consul", "Consul must not be collected unless asked for")
	require.NotContains(t, argv, "-vault")
}

func TestOptionalProductsAreRequestedWhenAsked(t *testing.T) {
	binary := fakeHCDiag(t, "exit 0\n")
	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = t.TempDir()
	})

	call(t, p, map[string]any{"include_consul": true, "include_vault": true})

	argv := argvOf(t, binary)
	require.Contains(t, argv, "-consul")
	require.Contains(t, argv, "-vault")
}

func TestInvalidSinceIsRejectedBeforeAnythingRuns(t *testing.T) {
	binary := fakeHCDiag(t, "exit 0\n")
	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = t.TempDir()
	})

	_, isErr, text := call(t, p, map[string]any{"since": "yesterday"})
	require.True(t, isErr)
	require.Contains(t, text, "Go duration")

	_, err := os.Stat(filepath.Join(filepath.Dir(binary), "argv"))
	require.Error(t, err, "an invalid argument must be caught before hcdiag is executed")
}

func TestDryRunReportsThatNothingWasCollected(t *testing.T) {
	binary := fakeHCDiag(t, "echo 'would run: nomad debug'\nexit 0\n")
	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = t.TempDir()
	})

	out, isErr, _ := call(t, p, map[string]any{"dry_run": true})
	require.False(t, isErr)
	require.Equal(t, true, out["dry_run"])
	require.Contains(t, out["note"], "Nothing was collected")
	require.Contains(t, argvOf(t, binary), "-dryrun")
	require.NotContains(t, out, "bundle_path")
}

func TestBundleIsFoundAndReportedByPathOnly(t *testing.T) {
	dest := t.TempDir()
	binary := fakeHCDiag(t, "printf 'x' > \"$(echo \"$*\" | sed 's/.*-dest //;s/ .*//')/hcdiag-2026.tar.gz\"\nexit 0\n")

	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = dest
	})

	out, isErr, text := call(t, p, nil)
	require.False(t, isErr, text)

	require.Equal(t, filepath.Join(dest, "hcdiag-2026.tar.gz"), out["bundle_path"])
	require.EqualValues(t, 1, out["bundle_bytes"])

	// The contents must never come back. This is the whole point of the tool
	// returning a path.
	require.NotContains(t, out, "bundle_contents")
	require.Contains(t, out["note"], "has NOT been read")
	require.Contains(t, out["note"], "credentials")
}

// The token is passed through the environment, never on the command line,
// where `ps` would show it to every other user on the machine.
func TestTokenIsNeverPutOnTheCommandLine(t *testing.T) {
	dest := t.TempDir()
	// Deliberately not shaped as "token=..." — the redactor has a rule for
	// that form too, and this test is about the literal value being stripped.
	binary := fakeHCDiag(t, "echo \"got <$NOMAD_TOKEN> at <$NOMAD_ADDR>\"\nexit 0\n")

	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = dest
	})

	out, _, _ := call(t, p, nil)

	require.NotContains(t, argvOf(t, binary), "s3cret-token-value",
		"the token must not appear in argv")

	// It did reach the child, through the environment.
	require.Contains(t, out["hcdiag_output"], "at <http://127.0.0.1:4646>")

	// And it must not come back out in the echoed output either.
	require.Contains(t, out["hcdiag_output"], "got <"+utils.Placeholder+">",
		"the redactor must replace the token in hcdiag's own output")
	require.NotContains(t, out["hcdiag_output"], "s3cret-token-value")
}

// hcdiag exits non-zero when some runners fail, having still written a usable
// bundle. Reporting that as a flat failure throws away a bundle that may well
// answer the question.
func TestPartialCollectionStillReportsTheBundle(t *testing.T) {
	dest := t.TempDir()
	binary := fakeHCDiag(t,
		"printf 'x' > \"$(echo \"$*\" | sed 's/.*-dest //;s/ .*//')/hcdiag-partial.tar.gz\"\n"+
			"echo '1 error occurred: nomad debug failed'\nexit 1\n")

	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = dest
	})

	out, isErr, text := call(t, p, nil)
	require.False(t, isErr, "a partial collection is not a failed one: %s", text)

	require.Equal(t, filepath.Join(dest, "hcdiag-partial.tar.gz"), out["bundle_path"])
	require.Equal(t, true, out["partial"])
	require.Contains(t, out["note"], "partial collection rather than a failed one")
}

func TestFailureWithNoBundleIsAnError(t *testing.T) {
	binary := fakeHCDiag(t, "echo 'could not reach Nomad'\nexit 1\n")
	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = t.TempDir()
	})

	_, isErr, text := call(t, p, nil)
	require.True(t, isErr)
	require.Contains(t, text, "no new bundle appeared")
	require.Contains(t, text, "could not reach Nomad", "hcdiag's own output explains why")
}

func TestATimeoutStopsTheChildAndSaysSo(t *testing.T) {
	binary := fakeHCDiag(t, "sleep 30\n")
	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = t.TempDir()
		c.HCDiagTimeout = 300_000_000 // 300ms
	})

	_, isErr, text := call(t, p, nil)
	require.True(t, isErr)
	require.Contains(t, text, "did not finish")
	require.Contains(t, text, "NOMAD_MCP_HCDIAG_TIMEOUT")
}

// Newer hcdiag puts collection behind a `run` subcommand; older releases have
// no subcommands. The modern form is tried first and the legacy one used only
// when the binary genuinely does not recognise it.
func TestLegacyInvocationIsUsedOnlyWhenRunIsUnrecognised(t *testing.T) {
	dest := t.TempDir()
	binary := fakeHCDiag(t, `
case "$1" in
  run) echo 'Unknown subcommand "run"'; exit 127 ;;
esac
printf 'x' > "$(echo "$*" | sed 's/.*-dest //;s/ .*//')/hcdiag-legacy.tar.gz"
exit 0
`)

	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = dest
	})

	out, isErr, text := call(t, p, nil)
	require.False(t, isErr, text)
	require.Equal(t, filepath.Join(dest, "hcdiag-legacy.tar.gz"), out["bundle_path"])

	require.NotContains(t, argvOf(t, binary), "run ",
		"the successful invocation should be the legacy one")
}

// A genuine failure must not be retried as a legacy invocation — that would run
// the collection twice.
func TestARealFailureIsNotRetried(t *testing.T) {
	dest := t.TempDir()
	counter := filepath.Join(dest, "runs")
	binary := fakeHCDiag(t,
		"echo x >> \""+counter+"\"\necho 'could not reach Nomad'\nexit 1\n")

	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = t.TempDir()
	})

	call(t, p, nil)

	data, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), "x"),
		"a real failure must be reported, not retried")
}

// The child gets a deliberately small environment rather than inheriting
// whatever this process was started with.
func TestChildEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("SOME_UNRELATED_SECRET", "must-not-be-inherited")

	binary := fakeHCDiag(t, "echo \"leaked=[$SOME_UNRELATED_SECRET]\"\nexit 0\n")
	p := providerFor(t, func(c *config.Config) {
		c.HCDiagPath = binary
		c.HCDiagDest = t.TempDir()
	})

	out, _, _ := call(t, p, nil)
	require.Contains(t, out["hcdiag_output"], "leaked=[]",
		"the child must not inherit this process's unrelated environment")
}
