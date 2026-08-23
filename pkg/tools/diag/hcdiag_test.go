// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package diag

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

// --- destination confinement ------------------------------------------------
//
// The destination is a tool argument, so without confinement anything driving
// this server could write a large file anywhere the process can write.

func TestDestinationDefaultsToTheConfiguredRoot(t *testing.T) {
	root := t.TempDir()

	dest, errMsg := resolveDestination(&config.Config{HCDiagDest: root}, "")
	require.Empty(t, errMsg)
	require.Equal(t, root, dest)
}

func TestDestinationDefaultsToTempWhenUnconfigured(t *testing.T) {
	dest, errMsg := resolveDestination(&config.Config{}, "")
	require.Empty(t, errMsg)
	require.Equal(t, mustAbs(t, os.TempDir()), dest)
}

func TestDestinationOutsideTheRootIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	_, errMsg := resolveDestination(&config.Config{HCDiagDest: root}, outside)
	require.Contains(t, errMsg, "Refused")
	require.Contains(t, errMsg, "NOMAD_MCP_HCDIAG_DEST")
	require.Contains(t, errMsg, "not something you can change from here",
		"the refusal must stop the model hunting for a workaround")
}

func TestDestinationTraversalIsRefused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")
	require.NoError(t, os.MkdirAll(root, 0o755))

	for _, attempt := range []string{
		"../..", "../elsewhere", "sub/../../..", filepath.Join(root, "..", "escape"),
	} {
		_, errMsg := resolveDestination(&config.Config{HCDiagDest: root}, attempt)
		require.Contains(t, errMsg, "Refused", "%q should not escape the root", attempt)
	}
}

// A sibling directory whose name merely starts with the root's name is not
// inside it. A string-prefix check would get this wrong.
func TestDestinationSiblingWithASharedPrefixIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "bundles")
	sibling := filepath.Join(base, "bundles-elsewhere")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))

	_, errMsg := resolveDestination(&config.Config{HCDiagDest: root}, sibling)
	require.Contains(t, errMsg, "Refused")
}

func TestRelativeDestinationResolvesInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "today"), 0o755))

	dest, errMsg := resolveDestination(&config.Config{HCDiagDest: root}, "today")
	require.Empty(t, errMsg)
	require.Equal(t, filepath.Join(root, "today"), dest)
}

func TestDestinationMustExistAndBeADirectory(t *testing.T) {
	root := t.TempDir()

	_, errMsg := resolveDestination(&config.Config{HCDiagDest: root}, "does-not-exist")
	require.Contains(t, errMsg, "cannot be used")

	file := filepath.Join(root, "a-file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	_, errMsg = resolveDestination(&config.Config{HCDiagDest: root}, file)
	require.Contains(t, errMsg, "is a file, not a directory")
}

func TestWithinRoot(t *testing.T) {
	require.True(t, withinRoot("/tmp/b", "/tmp/b"))
	require.True(t, withinRoot("/tmp/b", "/tmp/b/c"))
	require.True(t, withinRoot("/tmp/b", "/tmp/b/c/d"))
	require.False(t, withinRoot("/tmp/b", "/tmp"))
	require.False(t, withinRoot("/tmp/b", "/tmp/bb"))
	require.False(t, withinRoot("/tmp/b", "/etc/passwd"))
}

// --- finding the bundle -----------------------------------------------------
//
// The bundle is found by diffing the directory rather than by predicting
// hcdiag's filename, which has changed between releases.

func TestNewArchiveIgnoresWhatWasAlreadyThere(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hcdiag-old.tar.gz")

	before := archivesIn(dir)
	require.Len(t, before, 1)

	started := time.Now()
	writeFile(t, dir, "hcdiag-new.tar.gz")

	found := newArchiveIn(dir, before, started)
	require.Equal(t, filepath.Join(dir, "hcdiag-new.tar.gz"), found)
}

func TestNewArchiveReturnsNothingWhenNoneWasWritten(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hcdiag-old.tar.gz")

	require.Empty(t, newArchiveIn(dir, archivesIn(dir), time.Now()))
}

func TestNewArchiveIgnoresFilesThatAreNotArchives(t *testing.T) {
	dir := t.TempDir()
	started := time.Now()

	writeFile(t, dir, "notes.txt")
	writeFile(t, dir, "manifest.json")

	require.Empty(t, newArchiveIn(dir, map[string]bool{}, started))
}

func TestNewArchivePicksTheMostRecentOfSeveral(t *testing.T) {
	dir := t.TempDir()
	started := time.Now()

	writeFile(t, dir, "hcdiag-a.tar.gz")
	older := time.Now().Add(-30 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "hcdiag-a.tar.gz"), older, older))

	writeFile(t, dir, "hcdiag-b.tgz")

	require.Equal(t, filepath.Join(dir, "hcdiag-b.tgz"),
		newArchiveIn(dir, map[string]bool{}, started.Add(-time.Minute)))
}

// --- the manifest -----------------------------------------------------------

func TestManifestIsReadFromInsideTheBundle(t *testing.T) {
	bundle := writeBundle(t, map[string]string{
		manifestName: `{
			"started_at": "2026-08-23T10:00:00Z",
			"ended_at":   "2026-08-23T10:04:00Z",
			"duration":   "4m0s",
			"num_ops":    42,
			"version":    "0.5.4",
			"ops": {
				"nomad": [
					{"name": "nomad version",   "status": "success"},
					{"name": "nomad debug",     "status": "fail"},
					{"name": "nomad agent-info","status": "success"}
				],
				"host": [{"name": "uname", "status": "success"}]
			}
		}`,
	})

	summary, err := readManifest(bundle)
	require.NoError(t, err)
	require.Equal(t, 42, summary.NumOps)
	require.Equal(t, "0.5.4", summary.Version)
	require.Equal(t, "4m0s", summary.Duration)
	require.Equal(t, map[string]int{"nomad": 3, "host": 1}, summary.ByProduct)

	// A bundle missing the thing you needed looks identical to a complete one
	// unless the failures are named.
	require.Equal(t, []string{"nomad: nomad debug"}, summary.Failed)
}

// The manifest also carries the invoking username, the hostname and the full
// resolved configuration. The projection is an allowlist so none of that can
// reach the model's context.
func TestManifestSummaryDropsHostDetail(t *testing.T) {
	bundle := writeBundle(t, map[string]string{
		manifestName: `{
			"num_ops": 1,
			"environment": {"username": "someone", "hostname": "prod-box-01"},
			"configuration": {"nomad": true, "token": "s3cret"}
		}`,
	})

	summary, err := readManifest(bundle)
	require.NoError(t, err)

	encoded, err := json.Marshal(summary)
	require.NoError(t, err)

	for _, leaked := range []string{"someone", "prod-box-01", "s3cret"} {
		require.NotContains(t, string(encoded), leaked,
			"the manifest projection must not carry host detail into the context")
	}
}

func TestManifestMissingFromBundleIsNotAPanic(t *testing.T) {
	bundle := writeBundle(t, map[string]string{"results.json": `{}`})

	_, err := readManifest(bundle)
	require.Error(t, err)
}

func TestUnreadableBundleIsNotAPanic(t *testing.T) {
	dir := t.TempDir()
	notAnArchive := filepath.Join(dir, "hcdiag.tar.gz")
	require.NoError(t, os.WriteFile(notAnArchive, []byte("not gzip"), 0o600))

	_, err := readManifest(notAnArchive)
	require.Error(t, err)
}

// --- messages ---------------------------------------------------------------

func TestDisabledMessageNamesTheFlagAndForbidsRetrying(t *testing.T) {
	msg := disabledMessage()

	require.Contains(t, msg, "NOMAD_MCP_ENABLE_HCDIAG=true")
	require.Contains(t, msg, "--enable-hcdiag=true")
	require.Contains(t, msg, "do not retry")
	require.Contains(t, msg, "read_allocation_logs",
		"the refusal should point at the tools that are available")
}

func TestNotInstalledMessageExplainsWhereItGoes(t *testing.T) {
	msg := notInstalledMessage("hcdiag")

	require.Contains(t, msg, "not bundled with this server")
	require.Contains(t, msg, "NOMAD_MCP_HCDIAG_PATH")
	require.Contains(t, msg, "this host — not on the Nomad servers",
		"which machine needs hcdiag is the thing people get wrong")
}

// --- subcommand fallback ----------------------------------------------------

func TestUnknownSubcommandDetection(t *testing.T) {
	for _, output := range []string{
		"Unknown subcommand \"run\"",
		"flag provided but not defined: -run",
		"unknown command: run",
	} {
		require.True(t, looksLikeUnknownSubcommand(output), "%q should trigger the fallback", output)
	}

	for _, output := range []string{
		"error: could not reach Nomad at http://127.0.0.1:4646",
		"1 error occurred: nomad debug failed",
		"",
	} {
		require.False(t, looksLikeUnknownSubcommand(output),
			"%q is a real failure and must not be retried as a legacy invocation", output)
	}
}

// --- output shaping ---------------------------------------------------------

func TestConfigPathIsReducedToItsBasename(t *testing.T) {
	args := redactArgs([]string{"-nomad", "-config", "/home/someone/private/hcdiag.hcl"})

	require.Equal(t, "hcdiag.hcl", args[2])
	require.NotContains(t, strings.Join(args, " "), "/home/someone")
}

func TestHumanBytes(t *testing.T) {
	require.Equal(t, "512 B", humanBytes(512))
	require.Equal(t, "1.0 KB", humanBytes(1024))
	require.Equal(t, "1.5 MB", humanBytes(1024*1024*3/2))
}

// --- helpers ----------------------------------------------------------------

func mustAbs(t *testing.T, path string) string {
	t.Helper()

	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	return abs
}

func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
}

// writeBundle builds a real .tar.gz, so readManifest is exercised against the
// archive format rather than a stub.
func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "hcdiag-test.tar.gz")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     filepath.Join("hcdiag-test", name),
			Mode:     0o600,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return path
}
