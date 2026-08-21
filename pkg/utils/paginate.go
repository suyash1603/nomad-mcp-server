// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
)

// Pagination defaults. DefaultPerPage is deliberately modest: a list tool's
// result lands directly in the model's context, and a cluster with thousands of
// allocations would otherwise fill it in one call.
const (
	DefaultPerPage = 50
	MaxPerPage     = 500
)

// Page holds the pagination arguments of a list tool call.
type Page struct {
	PerPage   int32
	NextToken string
}

// PageParams returns the tool options declaring per_page and next_token.
// Every list tool includes these, so the arguments are named identically
// everywhere.
func PageParams() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithNumber("per_page",
			mcp.DefaultNumber(DefaultPerPage),
			mcp.Description(
				"Maximum number of results to return in this call. Defaults to 50, maximum 500. "+
					"Keep this small unless you genuinely need every result: large pages consume a lot of context."),
		),
		mcp.WithString("next_token",
			mcp.Description(
				"Pagination cursor. Pass the next_token value from a previous response to fetch the following page. "+
					"Omit it for the first page. If a response has no next_token, there are no more results."),
		),
	}
}

// PageFrom reads the pagination arguments from a tool call, clamping per_page
// into range so a malformed or over-eager value cannot blow up the response.
func PageFrom(req mcp.CallToolRequest) Page {
	perPage := req.GetInt("per_page", DefaultPerPage)
	switch {
	case perPage <= 0:
		perPage = DefaultPerPage
	case perPage > MaxPerPage:
		perPage = MaxPerPage
	}

	return Page{
		PerPage:   int32(perPage),
		NextToken: req.GetString("next_token", ""),
	}
}

// Apply sets the pagination fields on a Nomad query.
func (p Page) Apply(q *api.QueryOptions) *api.QueryOptions {
	if q == nil {
		q = &api.QueryOptions{}
	}
	q.PerPage = p.PerPage
	q.NextToken = p.NextToken
	return q
}

// NextTokenNote explains an available next page in words as well as in the
// next_token field.
//
// A model that receives a truncated list with no explanation frequently reports
// the partial result as if it were complete. Saying so explicitly is what stops
// "you have 50 allocations" when there are 500.
func NextTokenNote(next string, returned int) string {
	if next == "" {
		return ""
	}
	return "More results exist beyond the " +
		itoa(returned) +
		" returned here. Pass next_token to this tool again to fetch the next page."
}

// itoa avoids pulling strconv into every caller.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
