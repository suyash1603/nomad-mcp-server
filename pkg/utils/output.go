// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// List is the envelope every list tool returns.
//
// The shape is identical across domains on purpose: a model that has learned to
// read one list tool's output can read all of them, and the presence of
// next_token is a consistent signal that more results exist.
type List struct {
	Count     int    `json:"count"`
	Namespace string `json:"namespace,omitempty"`
	Region    string `json:"region,omitempty"`
	NextToken string `json:"next_token,omitempty"`
	Note      string `json:"note,omitempty"`
	Items     any    `json:"items"`
}

// JSONResult marshals v and returns it as a tool result.
//
// Output is compact rather than indented. Indentation is pure overhead here:
// no human reads it directly, and every space costs context window.
func JSONResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError(
			fmt.Sprintf("Failed to encode the result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// ErrorResult returns msg as a tool error.
//
// The Go error is always nil: an MCP tool reports a recoverable failure through
// the result, so the model sees the explanation and can act on it. Returning a
// Go error instead surfaces a transport-level failure the model cannot read.
func ErrorResult(msg string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(msg), nil
}

// ErrorResultf is ErrorResult with formatting.
func ErrorResultf(format string, args ...any) (*mcp.CallToolResult, error) {
	return ErrorResult(fmt.Sprintf(format, args...))
}

// Truncated is the outcome of capping a body of text.
type Truncated struct {
	Content       string `json:"content"`
	Truncated     bool   `json:"truncated"`
	OriginalBytes int    `json:"original_bytes,omitempty"`
	ReturnedBytes int    `json:"returned_bytes,omitempty"`
	Note          string `json:"note,omitempty"`
}

// TruncateTail caps s at max bytes, keeping the **end** of the text.
//
// Keeping the tail rather than the head is the right default for logs: the
// interesting part of a failing task's output — the panic, the stack trace, the
// last thing it said before dying — is at the end. Keeping the head would
// reliably return a task's startup banner and drop the actual failure.
//
// The cut is moved forward to the next newline where possible, so the result
// starts at a line boundary rather than mid-line.
func TruncateTail(s string, max int64) Truncated {
	if max <= 0 || int64(len(s)) <= max {
		return Truncated{Content: s, Truncated: false}
	}

	original := len(s)
	cut := int64(original) - max
	tail := s[cut:]

	// Prefer starting on a line boundary, but only if that boundary is near
	// the cut; otherwise a single very long line would discard everything.
	if idx := indexByte(tail, '\n'); idx >= 0 && idx < 1024 {
		tail = tail[idx+1:]
	}

	return Truncated{
		Content:       tail,
		Truncated:     true,
		OriginalBytes: original,
		ReturnedBytes: len(tail),
		Note: fmt.Sprintf(
			"Output was %d bytes and has been truncated to the last %d bytes. "+
				"Earlier content was dropped. Raise NOMAD_MCP_MAX_LOG_BYTES, or narrow the request, to see more.",
			original, len(tail)),
	}
}

// TruncateHead caps s at max bytes, keeping the beginning. Used where the start
// is what matters, such as a file listing or a job specification.
func TruncateHead(s string, max int64) Truncated {
	if max <= 0 || int64(len(s)) <= max {
		return Truncated{Content: s, Truncated: false}
	}

	original := len(s)
	head := s[:max]

	if idx := lastIndexByte(head, '\n'); idx >= 0 && int64(len(head)-idx) < 1024 {
		head = head[:idx]
	}

	return Truncated{
		Content:       head,
		Truncated:     true,
		OriginalBytes: original,
		ReturnedBytes: len(head),
		Note: fmt.Sprintf(
			"Output was %d bytes and has been truncated to the first %d bytes. "+
				"Raise NOMAD_MCP_MAX_LOG_BYTES, or narrow the request, to see more.",
			original, len(head)),
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// FormatTime renders a Nomad timestamp as RFC3339 plus a relative age.
//
// Nomad returns UnixNano integers, which a model cannot reason about. The
// relative form ("14m ago") is what actually matters when triaging: whether a
// job was submitted minutes or weeks ago changes the diagnosis.
func FormatTime(unixNano int64) string {
	if unixNano <= 0 {
		return ""
	}
	t := time.Unix(0, unixNano).UTC()
	return fmt.Sprintf("%s (%s)", t.Format(time.RFC3339), RelativeAge(t))
}

// FormatUnixSeconds is FormatTime for second-resolution timestamps.
func FormatUnixSeconds(unixSec int64) string {
	if unixSec <= 0 {
		return ""
	}
	t := time.Unix(unixSec, 0).UTC()
	return fmt.Sprintf("%s (%s)", t.Format(time.RFC3339), RelativeAge(t))
}

// RelativeAge renders how long ago t was, in a form suited to reading rather
// than arithmetic.
func RelativeAge(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		return "in the future"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ShortID truncates a Nomad UUID to its first segment, the form the `nomad` CLI
// displays and the form humans use when referring to an allocation.
func ShortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
