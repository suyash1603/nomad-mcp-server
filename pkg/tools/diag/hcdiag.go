// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package diag wraps HashiCorp's hcdiag support-bundle collector.
//
// This is the only tool in the server that executes a local binary rather than
// calling Nomad's HTTP API, which makes it the only one with a meaningful local
// attack surface. Four decisions contain that, and none of them are optional:
//
//   - It is off unless NOMAD_MCP_ENABLE_HCDIAG is set. Every other tool is
//     governed by the read-only gate; this one carries its own switch because
//     the read-only gate is about the cluster and this is about the host.
//   - The binary is named by configuration, never by a tool argument. A model
//     that could choose the executable could run anything the server can.
//   - The command is built as an argv slice and handed to exec.Command, which
//     does not involve a shell. There is no string for an argument to break out
//     of.
//   - Credentials reach hcdiag through the environment, not the command line,
//     so they are not visible in `ps` to every other user on the machine.
//
// The bundle's contents are never returned. hcdiag collects configuration
// files, agent logs and command output, which on a real cluster means secrets;
// putting that in a model's context would defeat every other precaution here.
// The tool returns the path, the size, and a projection of the manifest.
package diag

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/config"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// maxOutputBytes caps how much of hcdiag's own stdout and stderr is returned.
// A dry run lists every runner it would execute, which on a busy host is long,
// and none of it is worth a context window.
const maxOutputBytes = 8192

// manifestName is the file hcdiag writes into the bundle describing the run.
// Reading it is how this tool reports what was collected without unpacking
// anything sensitive.
const manifestName = "manifest.json"

// CollectHCDiag runs hcdiag against the Nomad cluster and reports the bundle.
func CollectHCDiag(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("collect_hcdiag",
			mcp.WithDescription(
				"Collect an hcdiag support bundle for this Nomad cluster.\n\n"+
					"hcdiag is HashiCorp's own diagnostics collector. It gathers the things a "+
					"support engineer asks for and that are tedious to assemble by hand: agent "+
					"configuration, host information, Nomad's debug bundle, metrics, and recent "+
					"logs from the servers and clients. The result is a single .tar.gz.\n\n"+
					"Reach for this when a problem is beyond what the read tools explain, when "+
					"someone asks for \"an hcdiag\" or \"a support bundle\", or before opening a "+
					"support ticket. For a specific question — why a job will not place, why a "+
					"task is crash-looping — the individual tools answer faster and put the answer "+
					"in front of you; hcdiag produces an archive for a human to work through.\n\n"+
					"IMPORTANT — what this does and does not return. It runs a binary on the "+
					"machine hosting this MCP server and writes a file there. It returns the "+
					"bundle's PATH and a summary of what was collected. It does NOT return the "+
					"contents, and you must not try to read them: the bundle contains "+
					"configuration files, environment variables and logs, which on a real cluster "+
					"means credentials. Give the path to the user and let them decide what happens "+
					"to it.\n\n"+
					"Collection takes minutes rather than seconds, and the default 72-hour window "+
					"is what makes it slow. Narrow it with 'since' when the problem is recent — "+
					"'1h' after an incident this morning collects far less and finishes far "+
					"sooner. Use dry_run=true first to see what would be gathered without "+
					"gathering it.\n\n"+
					"This tool is disabled unless the operator has set NOMAD_MCP_ENABLE_HCDIAG, "+
					"because it is the only tool here that executes a local program."),
			// Read-only in this server's sense, which is the sense its gate
			// enforces: hcdiag changes nothing in the Nomad cluster. It does
			// write a bundle to local disk, which the description says plainly
			// and which NOMAD_MCP_ENABLE_HCDIAG is the actual gate for.
			//
			// Annotating it mutating instead would block it in read-only mode,
			// and that is worse than it sounds: an operator who wanted
			// diagnostics would have to enable writes to get them, unlocking
			// purge_node and delete_namespace to collect a support bundle.
			utils.ReadOnlyTool(),
			mcp.WithString("since",
				mcp.DefaultString("72h"),
				mcp.Description(
					"How far back to collect logs and metrics, as a Go duration such as \"72h\", "+
						"\"4h\" or \"30m\". Defaults to 72h, which is hcdiag's own default and the "+
						"main reason a run is slow. Narrow it when the problem is recent."),
			),
			mcp.WithBoolean("dry_run",
				mcp.DefaultBool(false),
				mcp.Description(
					"List what would be collected without running anything or writing a bundle. "+
						"Fast, and the right first call."),
			),
			mcp.WithBoolean("include_consul",
				mcp.DefaultBool(false),
				mcp.Description(
					"Also collect Consul diagnostics. Worth setting when Nomad uses Consul for "+
						"service discovery and the symptom involves service registration or mesh "+
						"connectivity. Requires the Consul CLI and credentials on this host."),
			),
			mcp.WithBoolean("include_vault",
				mcp.DefaultBool(false),
				mcp.Description(
					"Also collect Vault diagnostics. Worth setting when tasks get their secrets "+
						"from Vault and the symptom is templates failing to render. Requires the "+
						"Vault CLI and credentials on this host."),
			),
			mcp.WithString("destination",
				mcp.Description(
					"Directory to write the bundle into. Defaults to the server's configured "+
						"destination, or the system temp directory. The operator may confine this "+
						"with NOMAD_MCP_HCDIAG_DEST, in which case a path outside it is refused."),
			),
			mcp.WithString("config_file",
				mcp.Description(
					"Path to an hcdiag HCL configuration file, for a collection the flags cannot "+
						"express — extra files, custom commands, additional redactions. Advanced; "+
						"leave unset unless the operator supplied one."),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return collect(ctx, req, p)
		},
	}
}

func collect(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	cfg := p.Config()

	if !cfg.EnableHCDiag {
		return utils.ErrorResult(disabledMessage())
	}

	binary, err := exec.LookPath(cfg.HCDiagPath)
	if err != nil {
		return utils.ErrorResult(notInstalledMessage(cfg.HCDiagPath))
	}

	since := strings.TrimSpace(req.GetString("since", "72h"))
	if _, err := time.ParseDuration(since); err != nil {
		return utils.ErrorResultf(
			"Invalid since %q: use a Go duration such as \"72h\", \"4h\" or \"30m\".", since)
	}

	dryRun := req.GetBool("dry_run", false)

	dest, errMsg := resolveDestination(cfg, req.GetString("destination", ""))
	if errMsg != "" {
		return utils.ErrorResult(errMsg)
	}

	args := []string{"-nomad", "-since", since, "-dest", dest}
	if req.GetBool("include_consul", false) {
		args = append(args, "-consul")
	}
	if req.GetBool("include_vault", false) {
		args = append(args, "-vault")
	}
	if dryRun {
		args = append(args, "-dryrun")
	}
	if configFile := strings.TrimSpace(req.GetString("config_file", "")); configFile != "" {
		if _, err := os.Stat(configFile); err != nil {
			return utils.ErrorResultf(
				"The hcdiag configuration file %q cannot be read: %v", configFile, err)
		}
		args = append(args, "-config", configFile)
	}

	// The bundle is found by comparing the directory before and after, rather
	// than by predicting hcdiag's filename. The name has changed between
	// releases and is not part of any contract.
	before := archivesIn(dest)
	startedAt := time.Now()

	runCtx, cancel := context.WithTimeout(ctx, cfg.HCDiagTimeout)
	defer cancel()

	result := run(runCtx, binary, args, p.NomadEnv(ctx), p.Redactor())
	elapsed := time.Since(startedAt)

	p.Logger().WithField("hcdiag", binary).
		WithField("dry_run", dryRun).
		WithField("since", since).
		WithField("duration", elapsed.Round(time.Second).String()).
		WithField("exit_code", result.exitCode).
		Info("ran hcdiag")

	out := map[string]any{
		"command":  cfg.HCDiagPath + " " + strings.Join(redactArgs(args), " "),
		"duration": elapsed.Round(time.Second).String(),
		"since":    since,
	}
	if result.output != "" {
		out["hcdiag_output"] = result.output
	}

	if dryRun {
		out["dry_run"] = true
		out["note"] = "Nothing was collected and no bundle was written. This is the list of what " +
			"a real run would gather. Run again with dry_run=false to collect it."
		return utils.JSONResult(out)
	}

	if result.timedOut {
		out["note"] = fmt.Sprintf(
			"hcdiag did not finish within %s and was stopped. A partial or missing bundle is the "+
				"likely result. Narrow the window with 'since', or raise the limit with "+
				"NOMAD_MCP_HCDIAG_TIMEOUT.", cfg.HCDiagTimeout)
		return utils.ErrorResult(mustJSON(out))
	}

	// hcdiag exits non-zero when some runners fail, having still written a
	// usable bundle for the rest. Reporting that as a flat failure would throw
	// away a bundle that answers the question, so the bundle is looked for
	// either way and the exit code is reported alongside it.
	bundle := newArchiveIn(dest, before, startedAt)

	if bundle == "" {
		if result.err != nil {
			out["error"] = result.err.Error()
		}
		out["note"] = "hcdiag ran but no new bundle appeared in " + dest + ". Its output above " +
			"should say why; a missing Nomad CLI on this host, or a Nomad it could not reach, " +
			"are the usual causes."
		return utils.ErrorResult(mustJSON(out))
	}

	out["bundle_path"] = bundle
	if info, err := os.Stat(bundle); err == nil {
		out["bundle_bytes"] = info.Size()
		out["bundle_size"] = humanBytes(info.Size())
	}
	if summary, err := readManifest(bundle); err == nil && summary != nil {
		out["collected"] = summary
	}

	if result.exitCode != 0 {
		out["partial"] = true
		out["note"] = fmt.Sprintf(
			"hcdiag exited %d, which means some of what it tried to collect failed — but it still "+
				"wrote a bundle, and the rest of the data is in it. Its output above names what "+
				"failed. Treat this as a partial collection rather than a failed one.",
			result.exitCode)
	} else {
		out["note"] = "The bundle is on the machine running this MCP server, at the path above. " +
			"It has NOT been read and must not be: it contains agent configuration, environment " +
			"variables and logs, which on a real cluster means credentials. Give the path to the " +
			"user. hcdiag applies its own redactions, but they are a safety net, not a guarantee — " +
			"anyone sending this to a third party should look inside it first."
	}

	return utils.JSONResult(out)
}

// runResult is the outcome of one hcdiag invocation.
type runResult struct {
	output   string
	exitCode int
	timedOut bool
	err      error
}

// run executes hcdiag and captures its output.
//
// Newer hcdiag puts the collection behind a `run` subcommand and deprecates the
// bare form; older releases have no subcommands at all. Rather than pin a
// version, the modern form is tried first and the legacy one is used if the
// binary does not recognise it.
func run(ctx context.Context, binary string, args, env []string, r *utils.Redactor) runResult {
	result := exec1(ctx, binary, append([]string{"run"}, args...), env, r)
	if result.exitCode != 0 && looksLikeUnknownSubcommand(result.output) {
		return exec1(ctx, binary, args, env, r)
	}
	return result
}

// exec1 runs the binary once with an explicit argv. There is no shell here and
// there must never be one: every argument arrives from a tool call.
func exec1(ctx context.Context, binary string, args, env []string, r *utils.Redactor) runResult {
	cmd := exec.CommandContext(ctx, binary, args...)

	// The child gets a deliberately small environment: what it needs to find
	// binaries and talk to Nomad, and nothing else this process happens to
	// have been started with.
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}, env...)

	combined, err := cmd.CombinedOutput()

	res := runResult{
		output: utils.TruncateTail(r.String(string(combined)), maxOutputBytes).Content,
		err:    err,
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.timedOut = true
	}

	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.exitCode = exitErr.ExitCode()
	case err != nil:
		res.exitCode = -1
	}
	return res
}

// looksLikeUnknownSubcommand recognises a binary too old to have `hcdiag run`.
func looksLikeUnknownSubcommand(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "unknown subcommand") ||
		strings.Contains(lower, "flag provided but not defined") ||
		strings.Contains(lower, "unknown command")
}

// resolveDestination picks the directory the bundle is written to and enforces
// the operator's confinement.
//
// The confinement matters because the destination is a tool argument: without
// it, anything driving this server could write a large file anywhere the
// process can write.
func resolveDestination(cfg *config.Config, requested string) (string, string) {
	root := cfg.HCDiagDest
	if root == "" {
		root = os.TempDir()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "The configured hcdiag destination could not be resolved: " + err.Error()
	}

	requested = strings.TrimSpace(requested)
	if requested == "" {
		return root, ""
	}

	dest := requested
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(root, dest)
	}
	dest = filepath.Clean(dest)

	// Only enforced when the operator has actually set a root. With no
	// NOMAD_MCP_HCDIAG_DEST the default is the temp directory and an absolute
	// path elsewhere is a legitimate choice.
	if cfg.HCDiagDest != "" && !withinRoot(root, dest) {
		return "", fmt.Sprintf(
			"Refused: %q is outside %q, and this server is configured to write hcdiag bundles "+
				"only under that directory (NOMAD_MCP_HCDIAG_DEST).\n\n"+
				"Choose a path inside it, or omit 'destination' to use it directly. This is not "+
				"something you can change from here.", requested, root)
	}

	if info, err := os.Stat(dest); err != nil {
		return "", fmt.Sprintf(
			"The destination %q cannot be used: %v. It must be a directory that already exists "+
				"and is writable by the user running this server.", dest, err)
	} else if !info.IsDir() {
		return "", fmt.Sprintf("The destination %q is a file, not a directory.", dest)
	}

	return dest, ""
}

// withinRoot reports whether path is root or lives underneath it. It compares
// cleaned absolute paths rather than string prefixes, so "/tmp/bundles-evil"
// does not count as being inside "/tmp/bundles".
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// archivesIn returns the archive files currently in dir, as a set.
func archivesIn(dir string) map[string]bool {
	found := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return found
	}
	for _, entry := range entries {
		if !entry.IsDir() && isArchive(entry.Name()) {
			found[entry.Name()] = true
		}
	}
	return found
}

// newArchiveIn returns the full path of the archive hcdiag just wrote.
//
// A file is only accepted if it was not there before AND was modified after the
// run started. Either test alone is too weak: a concurrent collection could
// have created one, and a file could have been replaced in place.
func newArchiveIn(dir string, before map[string]bool, after time.Time) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	type candidate struct {
		path string
		mod  time.Time
	}
	var found []candidate

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || before[name] || !isArchive(name) {
			continue
		}
		info, err := entry.Info()
		// A whole second of slack: some filesystems store mtimes at second
		// resolution, so a bundle written immediately after the start time can
		// otherwise appear to predate it.
		if err != nil || info.ModTime().Before(after.Add(-time.Second)) {
			continue
		}
		found = append(found, candidate{filepath.Join(dir, name), info.ModTime()})
	}

	if len(found) == 0 {
		return ""
	}
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	return found[0].path
}

func isArchive(name string) bool {
	return strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
}

// manifestSummary is the projection of hcdiag's manifest.
//
// The manifest also carries the invoking username, the hostname and the full
// resolved configuration. None of that helps anyone understand the collection,
// and all of it is host detail that should not be pulled into a model's
// context, so the projection is an allowlist rather than a redaction.
type manifestSummary struct {
	StartedAt string         `json:"started_at,omitempty"`
	EndedAt   string         `json:"ended_at,omitempty"`
	Duration  string         `json:"duration,omitempty"`
	NumOps    int            `json:"operations,omitempty"`
	Version   string         `json:"hcdiag_version,omitempty"`
	ByProduct map[string]int `json:"operations_by_product,omitempty"`
	Failed    []string       `json:"failed_operations,omitempty"`
}

// maxManifestBytes bounds what is read out of the archive. A manifest is tens
// of kilobytes; anything far larger is not one, and decompressing it into
// memory unbounded would be a trivial way to exhaust this process.
const maxManifestBytes = 4 << 20

// readManifest pulls hcdiag's manifest out of the bundle without extracting
// anything else. Nothing but the manifest is ever read.
func readManifest(bundle string) (*manifestSummary, error) {
	f, err := os.Open(bundle)
	if err != nil {
		return nil, err
	}
	// Both are read-only handles, so a close error has nothing to report and
	// nothing to recover; the linter wants the intent stated rather than
	// implied.
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no manifest in bundle")
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != manifestName {
			continue
		}

		data, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes))
		if err != nil {
			return nil, err
		}
		return summarise(data)
	}
}

// rawManifest mirrors only the manifest fields this tool reports.
type rawManifest struct {
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	Duration  string `json:"duration"`
	NumOps    int    `json:"num_ops"`
	Version   string `json:"version"`
	Ops       map[string][]struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"ops"`
}

func summarise(data []byte) (*manifestSummary, error) {
	var raw rawManifest
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	summary := &manifestSummary{
		StartedAt: raw.StartedAt,
		EndedAt:   raw.EndedAt,
		Duration:  raw.Duration,
		NumOps:    raw.NumOps,
		Version:   raw.Version,
	}
	if len(raw.Ops) == 0 {
		return summary, nil
	}

	summary.ByProduct = map[string]int{}
	for product, ops := range raw.Ops {
		summary.ByProduct[product] = len(ops)
		for _, op := range ops {
			// Naming what failed is the useful half: a bundle missing the
			// thing you needed looks identical to a complete one otherwise.
			if status := strings.ToLower(op.Status); status == "fail" || status == "unknown" {
				summary.Failed = append(summary.Failed, product+": "+op.Name)
			}
		}
	}
	sort.Strings(summary.Failed)
	return summary, nil
}

// redactArgs hides a config file path's directory in the echoed command. The
// rest of the arguments are values the caller supplied and are safe to repeat.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-config" {
			out[i+1] = filepath.Base(out[i+1])
		}
	}
	return out
}

func disabledMessage() string {
	return "Refused: collect_hcdiag is disabled on this server.\n\n" +
		"It is the only tool here that runs a program on the machine rather than calling " +
		"Nomad's API, so it is off unless the operator turns it on. This is not something you " +
		"can change from here, and no other tool collects a support bundle — do not retry or " +
		"look for a workaround.\n\n" +
		"To enable it, the person running this server must restart it with either:\n" +
		"  NOMAD_MCP_ENABLE_HCDIAG=true\n" +
		"  --enable-hcdiag=true\n\n" +
		"They will also need hcdiag installed on that machine. If the goal was to diagnose a " +
		"specific problem rather than to produce an archive, the read tools are all still " +
		"available and answer faster: read_evaluation for placement failures, " +
		"read_allocation_logs for a task that is failing, get_cluster_status and " +
		"check_connection for the cluster as a whole."
}

func notInstalledMessage(path string) string {
	return fmt.Sprintf(
		"collect_hcdiag is enabled, but the hcdiag binary %q was not found on the machine "+
			"running this MCP server.\n\n"+
			"hcdiag is a separate HashiCorp tool and is not bundled with this server. It has to be "+
			"installed on this host — not on the Nomad servers — because that is where this "+
			"process would run it:\n\n"+
			"  brew install hashicorp/tap/hcdiag\n"+
			"  # or download from https://releases.hashicorp.com/hcdiag/\n\n"+
			"If it is installed somewhere not on PATH, the operator can point at it with "+
			"NOMAD_MCP_HCDIAG_PATH. Note that hcdiag also shells out to the `nomad` CLI, so that "+
			"needs to be on this host too.\n\n"+
			"This is an operator action; you cannot fix it from here.", path)
}

// mustJSON renders a map for an error result. Errors are returned as text
// rather than structured content, so the map has to be marshalled by hand; a
// failure here falls back to the plain note, which is the part that matters.
func mustJSON(out map[string]any) string {
	data, err := json.Marshal(out)
	if err != nil {
		if note, ok := out["note"].(string); ok {
			return note
		}
		return "hcdiag failed, and the details could not be encoded."
	}
	return string(data)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
