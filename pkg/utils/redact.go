// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"regexp"
	"strings"
)

// Placeholder is what redacted values are replaced with.
const Placeholder = "[REDACTED]"

// minSecretLength guards literal redaction. Replacing every occurrence of a
// very short "secret" would corrupt unrelated output, and a Nomad ACL token is
// a 36-character UUID, so nothing legitimate is shorter than this.
const minSecretLength = 8

// labelledSecret matches "<label> = <value>" and "<label>: <value>" forms where
// the label names something secret. The value is captured separately so the
// label survives and the reader can still see *what* was redacted.
//
// AccessorID is deliberately absent: it identifies an ACL token but cannot
// authenticate as one, and it is often the useful part of an error message.
var labelledSecret = regexp.MustCompile(
	`(?i)\b(nomad_token|x-nomad-token|secret_?id|acl_?token|auth_?token|bootstrap_?token|token)\b(\s*["']?\s*[:=]\s*["']?)([^\s"',;}\]]+)`)

// bearerToken matches an Authorization header carrying a bearer token.
var bearerToken = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)(\S+)`)

// Redactor scrubs credentials out of strings before they reach a log line, an
// error message, or the model's context.
//
// It combines two strategies. Pattern redaction catches anything *labelled* as
// a secret, which covers values this process has never seen — a token echoed
// back inside a Nomad error body, for example. Literal redaction catches the
// specific secrets this process holds, which covers values that appear with no
// label at all.
//
// Deliberately absent: a rule matching bare UUIDs. Nomad allocation, evaluation,
// deployment and node IDs are all UUIDs, and redacting them would strip exactly
// the identifiers a user needs in order to debug anything.
type Redactor struct {
	literals []string
}

// NewRedactor returns a Redactor that scrubs the given literal secrets in
// addition to its built-in patterns. Empty and implausibly short values are
// ignored.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(strings.TrimSpace(s)) >= minSecretLength {
			r.literals = append(r.literals, s)
		}
	}
	return r
}

// String returns s with any credentials replaced by Placeholder.
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}

	// Literals first: a known secret may appear inside a value that the
	// pattern rules would otherwise leave alone.
	for _, lit := range r.literals {
		s = strings.ReplaceAll(s, lit, Placeholder)
	}

	s = replaceValue(labelledSecret, s, 3)
	s = replaceValue(bearerToken, s, 2)

	return s
}

// replaceValue rewrites capture group `value` of every match to Placeholder,
// leaving the rest of the match intact.
//
// A match whose value is already redacted is left alone, which makes the whole
// Redactor idempotent. That matters because redacted text is routinely passed
// through again — an error is scrubbed for a log line and then scrubbed again
// on its way to the model — and a naive replacement re-matches the placeholder
// itself, appending a bracket every time.
func replaceValue(re *regexp.Regexp, s string, value int) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		groups := re.FindStringSubmatch(match)
		if len(groups) <= value {
			return match
		}
		if strings.HasPrefix(groups[value], "[REDACT") {
			return match
		}

		// Rebuild the match with everything before the value preserved.
		prefixLen := len(match) - len(groups[value])
		return match[:prefixLen] + Placeholder
	})
}

// Error returns err's message with credentials scrubbed. A nil error yields an
// empty string.
func (r *Redactor) Error(err error) string {
	if err == nil {
		return ""
	}
	return r.String(err.Error())
}

// Fields scrubs the values of a log field map in place-safe fashion, returning
// a new map. Only string values are rewritten; everything else is copied.
func (r *Redactor) Fields(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = r.String(s)
			continue
		}
		out[k] = v
	}
	return out
}
