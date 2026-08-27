// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package investigate

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/tools/allocs"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// Defaults for a log search.
const (
	defaultMatchesPerAlloc = 10
	maxMatchesPerAlloc     = 100
	defaultSearchBytes     = 262144 // 256 KiB of tail per stream
)

// logMatch is one matching line.
type logMatch struct {
	AllocID  string `json:"alloc_id"`
	ShortID  string `json:"short_id"`
	Node     string `json:"node,omitempty"`
	Task     string `json:"task"`
	Stream   string `json:"stream"`
	Status   string `json:"client_status,omitempty"`
	Time     string `json:"time,omitempty"`
	Line     string `json:"line"`
	LineFrom int    `json:"line_from_end"`
}

// allocSearch is what searching one allocation produced.
type allocSearch struct {
	matches    []logMatch
	total      int
	scanned    int
	timestamps bool
}

// searchResult is the tool's output.
type searchResult struct {
	JobID           string              `json:"job_id"`
	Namespace       string              `json:"namespace"`
	Pattern         string              `json:"pattern"`
	Matches         []logMatch          `json:"matches"`
	MatchCount      int                 `json:"match_count"`
	TotalMatches    int                 `json:"total_matches"`
	AllocsSearched  int                 `json:"allocations_searched"`
	AllocsWithMatch int                 `json:"allocations_with_matches"`
	AllocsTotal     int                 `json:"allocations_total"`
	LinesScanned    int                 `json:"lines_scanned"`
	Unreachable     int                 `json:"allocations_unreachable,omitempty"`
	Errors          []utils.FanOutError `json:"errors,omitempty"`
	Note            string              `json:"note,omitempty"`
	TimeFilterNote  string              `json:"time_filter_note,omitempty"`
	Warning         string              `json:"warning"`
}

// SearchJobLogs greps the logs of every allocation of a job at once.
func SearchJobLogs(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("search_job_logs",
			mcp.WithDescription(
				"Search the task logs of every allocation of a job in one call, and return only the "+
					"lines that match.\n\n"+
					"This is the tool to reach for whenever a job has more than a handful of "+
					"allocations. Reading each allocation's logs one at a time costs a call per "+
					"allocation and fills the context with healthy replicas; this reads them "+
					"concurrently, matches on the server side, and returns the matching lines with "+
					"the allocation, node and task they came from.\n\n"+
					"Use it to answer \"which replica is throwing this error?\", \"is this happening "+
					"everywhere or on one node?\", and \"does this error appear at all?\".\n\n"+
					"IMPORTANT — what Nomad can and cannot do here. Logs are files on the client "+
					"node that Nomad rotates, so only recent output exists at all, and an "+
					"allocation that was rescheduled took its logs with it. This tool reads the "+
					"TAIL of each stream, not its whole history. If you need one allocation's full "+
					"log, use read_allocation_logs.\n\n"+
					"Time filtering is best-effort and depends entirely on the workload writing "+
					"timestamps at the start of its lines. When it does not, the time filter cannot "+
					"be applied and the result says so — read time_filter_note before describing "+
					"any result as covering a time range."),
			utils.ReadOnlyTool(),
			jobIDParam(),
			mcp.WithString("pattern",
				mcp.Required(),
				mcp.Description(
					"A regular expression (RE2 syntax) matched against each log line. "+
						"Plain text works as-is: \"connection refused\" matches that phrase. "+
						"Use alternation for several terms at once, for example "+
						"\"(?i)(timeout|refused|panic)\"."),
			),
			mcp.WithBoolean("case_sensitive",
				mcp.DefaultBool(false),
				mcp.Description("Match case-sensitively. Off by default, which is usually what you want in logs."),
			),
			mcp.WithString("log_type",
				mcp.DefaultString("both"),
				mcp.Enum("stdout", "stderr", "both"),
				mcp.Description(
					"Which stream to search. \"both\" is the default because an application's choice "+
						"of stream is not something you can assume."),
			),
			mcp.WithString("task",
				mcp.Description(
					"Restrict the search to one task by name. By default every task in the "+
						"allocation is searched, which matters when a group runs sidecars."),
			),
			mcp.WithNumber("max_matches_per_alloc",
				mcp.DefaultNumber(defaultMatchesPerAlloc),
				mcp.Description(
					"Stop after this many matches from any single allocation, so one very noisy "+
						"replica cannot crowd out the others. Defaults to 10, maximum 100."),
			),
			mcp.WithString("since",
				mcp.Description(
					"Only return lines whose leading timestamp is at or after this RFC3339 time, "+
						"for example \"2026-08-27T10:00:00Z\". Best-effort: it applies only to lines "+
						"that begin with a parseable timestamp. Lines without one are KEPT and "+
						"counted, and the result reports how many."),
			),
			mcp.WithString("until",
				mcp.Description(
					"Only return lines whose leading timestamp is at or before this RFC3339 time. "+
						"Same best-effort caveat as since."),
			),
			mcp.WithBoolean("all",
				mcp.DefaultBool(false),
				mcp.Description(
					"Include allocations from older job versions and previous deployments. Off by "+
						"default. Turn it on to search what was running before the most recent change."),
			),
			utils.AllocStatusParam(),
			utils.NamespaceParam(),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return searchJobLogs(ctx, req, p)
		},
	}
}

func searchJobLogs(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	jobID, namespace, region, nomad, errMsg := resolveJob(ctx, req, p)
	if errMsg != "" {
		return utils.ErrorResult(errMsg)
	}

	pattern, err := req.RequireString("pattern")
	if err != nil || strings.TrimSpace(pattern) == "" {
		return utils.ErrorResult(
			"The 'pattern' argument is required: it is the regular expression to search for. " +
				"Plain text works as a pattern, for example \"connection refused\".")
	}

	re, errMsg := compilePattern(pattern, req.GetBool("case_sensitive", false))
	if errMsg != "" {
		return utils.ErrorResult(errMsg)
	}

	since, until, errMsg := timeWindow(req)
	if errMsg != "" {
		return utils.ErrorResult(errMsg)
	}

	streams := []string{"stdout", "stderr"}
	if lt := req.GetString("log_type", "both"); lt != "both" {
		streams = []string{lt}
	}

	perAlloc := req.GetInt("max_matches_per_alloc", defaultMatchesPerAlloc)
	switch {
	case perAlloc <= 0:
		perAlloc = defaultMatchesPerAlloc
	case perAlloc > maxMatchesPerAlloc:
		perAlloc = maxMatchesPerAlloc
	}

	stubs, _, err := nomad.Jobs().Allocations(jobID, req.GetBool("all", false), &api.QueryOptions{
		Namespace: namespace,
		Region:    region,
	})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "list allocations for job " + jobID,
			Kind:       "job",
			Name:       jobID,
			Namespace:  namespace,
			Address:    p.Address(),
			Capability: "read-job",
			ListTool:   "list_jobs",
		}, p.Redactor()))
	}

	targets := selectSearchTargets(stubs, utils.AllocStatusFilter(req))
	if len(targets) == 0 {
		return utils.JSONResult(searchResult{
			JobID:       jobID,
			Namespace:   namespace,
			Pattern:     pattern,
			Matches:     []logMatch{},
			AllocsTotal: len(stubs),
			Note: "This job has no allocations whose logs could be searched. " +
				"An allocation that was never placed has no logs at all — run list_job_evaluations " +
				"to find out why. If you filtered by status, try removing it.",
			Warning: untrustedNote,
		})
	}

	wantTask := req.GetString("task", "")
	maxBytes := searchBytes(p)

	out := utils.FanOut(ctx, targets,
		utils.FanOutLimits{
			Concurrency: searchConcurrency,
			MaxTargets:  maxAllocationsSearched,
			Budget:      searchBudget,
		},
		func(ctx context.Context, stub *api.AllocationListStub) (allocSearch, error) {
			return searchOneAlloc(ctx, nomad, p, stub, searchSpec{
				re:        re,
				streams:   streams,
				task:      wantTask,
				namespace: namespace,
				region:    region,
				maxBytes:  maxBytes,
				perAlloc:  perAlloc,
				since:     since,
				until:     until,
			})
		})

	return utils.JSONResult(assembleSearch(jobID, namespace, pattern, len(stubs), since, until, out))
}

// searchSpec is what one allocation's search needs.
type searchSpec struct {
	re        *regexp.Regexp
	streams   []string
	task      string
	namespace string
	region    string
	maxBytes  int64
	perAlloc  int
	since     *time.Time
	until     *time.Time
}

// searchOneAlloc reads and matches every requested stream of one allocation.
func searchOneAlloc(
	ctx context.Context,
	nomad *api.Client,
	p *client.Provider,
	stub *api.AllocationListStub,
	spec searchSpec,
) (allocSearch, error) {
	var res allocSearch

	alloc, _, err := nomad.Allocations().Info(stub.ID, &api.QueryOptions{
		Namespace: spec.namespace,
		Region:    spec.region,
	})
	if err != nil {
		return res, fmt.Errorf("reading allocation %s: %w", utils.ShortID(stub.ID), err)
	}

	tasks := allocs.TaskNames(alloc)
	if spec.task != "" {
		tasks = []string{spec.task}
	}
	if len(tasks) == 0 {
		// Fall back to whatever the allocation actually reported state for,
		// which covers a job specification this token cannot read.
		for name := range stub.TaskStates {
			tasks = append(tasks, name)
		}
		sort.Strings(tasks)
	}

	for _, task := range tasks {
		for _, stream := range spec.streams {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}

			content, err := allocs.ReadTaskLogTail(ctx, nomad, alloc, task, stream,
				spec.namespace, spec.region, spec.maxBytes)
			if err != nil {
				// A missing stream is normal — a task that never wrote to
				// stdout has no stdout file — so this is not worth failing the
				// whole allocation over.
				continue
			}

			lines := allocs.SplitLogLines(content, true)
			res.scanned += len(lines)

			for i, line := range lines {
				if !spec.re.MatchString(line) {
					continue
				}

				ts, ok := leadingTimestamp(line)
				if ok {
					res.timestamps = true
					if !withinWindow(ts, spec.since, spec.until) {
						continue
					}
				}

				res.total++
				if len(res.matches) >= spec.perAlloc {
					continue
				}

				m := logMatch{
					AllocID:  stub.ID,
					ShortID:  utils.ShortID(stub.ID),
					Node:     stub.NodeName,
					Task:     task,
					Stream:   stream,
					Status:   stub.ClientStatus,
					Line:     p.Redactor().String(strings.TrimRight(line, "\r")),
					LineFrom: len(lines) - i,
				}
				if ok {
					m.Time = ts.UTC().Format(time.RFC3339)
				}
				res.matches = append(res.matches, m)
			}
		}
	}

	return res, nil
}

// assembleSearch folds the per-allocation results into the tool's output.
func assembleSearch(
	jobID, namespace, pattern string,
	allocsTotal int,
	since, until *time.Time,
	out utils.FanOutResult[allocSearch],
) searchResult {
	res := searchResult{
		JobID:          jobID,
		Namespace:      namespace,
		Pattern:        pattern,
		Matches:        []logMatch{},
		AllocsSearched: out.Visited,
		AllocsTotal:    allocsTotal,
		Unreachable:    out.Failed,
		Errors:         out.Errors,
		Warning:        untrustedNote,
	}

	var sawTimestamps bool
	for _, a := range out.Items {
		res.TotalMatches += a.total
		res.LinesScanned += a.scanned
		if len(a.matches) > 0 {
			res.AllocsWithMatch++
			res.Matches = append(res.Matches, a.matches...)
		}
		if a.timestamps {
			sawTimestamps = true
		}
	}
	res.MatchCount = len(res.Matches)

	// Allocations with matches first, so a truncated read still shows the
	// interesting ones. Within an allocation the original order is kept.
	sort.SliceStable(res.Matches, func(i, j int) bool {
		return res.Matches[i].ShortID < res.Matches[j].ShortID
	})

	res.Note = joinNote(out.Note, searchNote(res, pattern))
	if since != nil || until != nil {
		res.TimeFilterNote = timeFilterNote(sawTimestamps)
	}
	return res
}

// searchNote explains the shape of the result in words.
func searchNote(r searchResult, pattern string) string {
	switch {
	case r.TotalMatches == 0:
		return fmt.Sprintf(
			"No line matched %q in the %d allocations searched (%d lines). This means the pattern "+
				"does not appear in the RECENT logs those allocations still hold — it is not proof "+
				"the event never happened. Logs rotate, and a rescheduled allocation's logs are "+
				"gone entirely. Check list_job_allocations for allocations that no longer exist, "+
				"and build_job_timeline for events Nomad recorded independently of task output.",
			pattern, r.AllocsSearched, r.LinesScanned)

	case r.TotalMatches > r.MatchCount:
		return fmt.Sprintf(
			"%d lines matched in total; %d are returned here because the per-allocation cap "+
				"applied. Raise max_matches_per_alloc for more, or narrow the pattern.",
			r.TotalMatches, r.MatchCount)

	case r.AllocsWithMatch == 1 && r.AllocsSearched > 1:
		return fmt.Sprintf(
			"Only 1 of the %d allocations searched matched. A fault isolated to one allocation "+
				"often means the node it is on rather than the job — read_allocation will name it.",
			r.AllocsSearched)

	case r.AllocsWithMatch == r.AllocsSearched && r.AllocsSearched > 1:
		return fmt.Sprintf(
			"All %d allocations searched matched. A fault present on every replica points at the "+
				"job or a dependency it shares, not at any one node.",
			r.AllocsSearched)
	}
	return ""
}

// timeFilterNote states plainly whether the time window could be applied.
func timeFilterNote(sawTimestamps bool) string {
	if sawTimestamps {
		return "The time filter was applied to lines beginning with a parseable timestamp. " +
			"Lines without one were kept, because dropping them would silently hide output from " +
			"a workload that simply does not timestamp its logs. Treat the window as approximate."
	}
	return "NO parseable timestamps were found in any line, so the time filter had NO effect — " +
		"every matching line is returned regardless of when it was written. Nomad's log API has " +
		"no time-based filtering; a time window is only possible when the workload writes " +
		"timestamps itself. Do not describe this result as covering a time range."
}

// compilePattern builds the matcher, rejecting a bad expression in words the
// model can act on.
func compilePattern(pattern string, caseSensitive bool) (*regexp.Regexp, string) {
	expr := pattern
	if !caseSensitive {
		expr = "(?i)" + expr
	}

	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Sprintf(
			"The pattern %q is not a valid regular expression: %v. "+
				"Plain text is a valid pattern on its own; if you meant to search for a literal "+
				"character that is special in a regular expression — . * + ? ( ) [ ] { } | ^ $ \\ — "+
				"put a backslash in front of it.", pattern, err)
	}
	return re, ""
}

// timeWindow parses the optional since/until arguments.
func timeWindow(req mcp.CallToolRequest) (since, until *time.Time, errMsg string) {
	parse := func(name string) (*time.Time, string) {
		raw := strings.TrimSpace(req.GetString(name, ""))
		if raw == "" {
			return nil, ""
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, fmt.Sprintf(
				"The %q argument %q is not a valid RFC3339 timestamp. "+
					"Use a form like \"2026-08-27T10:00:00Z\".", name, raw)
		}
		return &t, ""
	}

	since, errMsg = parse("since")
	if errMsg != "" {
		return nil, nil, errMsg
	}
	until, errMsg = parse("until")
	if errMsg != "" {
		return nil, nil, errMsg
	}
	if since != nil && until != nil && until.Before(*since) {
		return nil, nil, "The 'until' time is before 'since', which cannot match anything."
	}
	return since, until, ""
}

// leadingTimestamp parses a timestamp at the start of a log line.
//
// Two shapes cover almost everything real workloads emit: a single token
// (RFC3339 and its variants) and a date and time separated by a space. Both are
// tried after stripping the delimiters that commonly wrap them, because
// "[2026-08-27T10:00:00Z]" is as common in logs as the bare form.
func leadingTimestamp(line string) (time.Time, bool) {
	line = strings.TrimLeft(line, "[(<\"' \t")
	if len(line) < 10 {
		return time.Time{}, false
	}

	fields := strings.Fields(line)
	if len(fields) == 0 {
		return time.Time{}, false
	}

	// One token: RFC3339 and friends.
	if t, ok := parseStamp(trimStamp(fields[0]), singleTokenLayouts); ok {
		return t, true
	}

	// Two tokens: a date, a space, then a time.
	if len(fields) > 1 {
		joined := trimStamp(fields[0]) + " " + trimStamp(fields[1])
		if t, ok := parseStamp(joined, twoTokenLayouts); ok {
			return t, true
		}
	}

	return time.Time{}, false
}

// singleTokenLayouts are timestamps that occupy one whitespace-delimited field.
var singleTokenLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.000Z",
	"2006-01-02T15:04:05",
}

// twoTokenLayouts are timestamps written as a date, a space, then a time.
var twoTokenLayouts = []string{
	"2006-01-02 15:04:05.000",
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05",
}

// trimStamp removes the delimiters that commonly wrap a timestamp.
func trimStamp(s string) string {
	return strings.Trim(s, "[](){}<>\"',")
}

// parseStamp tries each layout against an exact string.
//
// Matching the whole field rather than a fixed-length prefix is what keeps
// "2026-08-27T10:00:00Z] hello" from being read as a valid timestamp followed
// by junk.
func parseStamp(s string, layouts []string) (time.Time, bool) {
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// withinWindow reports whether a timestamp falls in the requested range.
func withinWindow(t time.Time, since, until *time.Time) bool {
	if since != nil && t.Before(*since) {
		return false
	}
	if until != nil && t.After(*until) {
		return false
	}
	return true
}

// selectSearchTargets picks the allocations worth reading logs from.
func selectSearchTargets(stubs []*api.AllocationListStub, wanted func(string) bool) []*api.AllocationListStub {
	out := make([]*api.AllocationListStub, 0, len(stubs))
	for _, s := range stubs {
		if s == nil {
			continue
		}
		if wanted != nil && !wanted(s.ClientStatus) {
			continue
		}
		// An allocation that never started has no log files, so reading it
		// costs a round trip to learn nothing.
		if s.ClientStatus == "pending" {
			continue
		}
		out = append(out, s)
	}

	// Failed and lost first: when the target cap trims this list, the
	// allocations most likely to explain a problem should be the ones that
	// survive, not whichever the API happened to return first.
	sort.SliceStable(out, func(i, j int) bool {
		return searchPriority(out[i].ClientStatus) < searchPriority(out[j].ClientStatus)
	})
	return out
}

func searchPriority(status string) int {
	switch status {
	case "failed":
		return 0
	case "lost":
		return 1
	case "running":
		return 2
	case "complete":
		return 3
	default:
		return 4
	}
}

// searchBytes is how much of each stream's tail is read.
//
// It is independent of NOMAD_MCP_MAX_LOG_BYTES, which caps what is returned to
// the model. Here the bytes are scanned and discarded, and only matching lines
// travel onward, so a small cap would reduce what can be found without saving
// any context. The configured cap is still honoured as a floor.
func searchBytes(p *client.Provider) int64 {
	if configured := p.Config().MaxLogBytes; configured > defaultSearchBytes {
		return configured
	}
	return defaultSearchBytes
}
