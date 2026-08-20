// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// enterpriseMarker is the body Nomad OSS returns for an Enterprise-only
// endpoint. Verified against Nomad 2.0.5 OSS:
//
//	GET /v1/quotas -> 501 "Nomad Enterprise only endpoint"
const enterpriseMarker = "nomad enterprise only endpoint"

// ErrorContext is what a tool knows about a failed call that the error itself
// does not. It exists because Nomad's own error bodies are extremely terse —
// a permission failure is the literal string "Permission denied", with no
// indication of which capability was missing or on what namespace. Verified
// against Nomad 2.0.5 OSS with ACLs enabled.
//
// The tool supplies that missing context; MapError turns it into a sentence a
// model can act on.
type ErrorContext struct {
	// Op describes what was attempted, as a verb phrase: "read job".
	Op string

	// Kind is the resource type, singular and lowercase: "job", "allocation".
	Kind string

	// Name is the identifier that was looked up.
	Name string

	// Namespace is the namespace the call was made against, if namespaced.
	Namespace string

	// Address is the Nomad address that was contacted, for connection errors.
	Address string

	// Capability is the Nomad ACL capability the call needs, such as
	// "read-job". Nomad will not tell us, so the tool must.
	Capability string

	// ListTool names the tool that would enumerate valid values, used to turn
	// a 404 into a next step: "try list_jobs".
	ListTool string
}

// MapError converts an error from the Nomad API into a message written for the
// model: what failed, why, and what to do next. It never returns a bare Go
// error and never leaks a credential.
//
// The returned string is safe to hand to mcp.NewToolResultError.
func MapError(err error, ec ErrorContext, r *Redactor) string {
	if err == nil {
		return ""
	}
	if r == nil {
		r = NewRedactor()
	}

	if msg, ok := mapConnectionError(err, ec); ok {
		return msg
	}

	code, body, ok := StatusCode(err)
	if !ok {
		return fmt.Sprintf("Failed to %s: %s", ec.Op, r.Error(err))
	}

	body = strings.TrimSpace(r.String(body))

	// An Enterprise-only endpoint is a 501, but check the body too: the same
	// marker can arrive on other codes depending on the endpoint.
	if strings.Contains(strings.ToLower(body), enterpriseMarker) || code == 501 {
		return fmt.Sprintf(
			"Cannot %s: this requires Nomad Enterprise, and the cluster at %s is running Nomad Community Edition.",
			ec.Op, ec.addressOrDefault())
	}

	switch code {
	case 400:
		return fmt.Sprintf("Cannot %s: Nomad rejected the request as invalid. %s", ec.Op, body)

	case 401:
		return fmt.Sprintf(
			"Cannot %s: Nomad requires an ACL token and none was provided. Set NOMAD_TOKEN in the environment the MCP server runs in.",
			ec.Op)

	case 403:
		return forbiddenMessage(ec)

	case 404:
		return notFoundMessage(ec, body)

	case 405:
		return fmt.Sprintf("Cannot %s: Nomad does not support that operation on this endpoint. %s", ec.Op, body)

	case 409:
		return fmt.Sprintf("Cannot %s: the request conflicts with the current state of the cluster. %s", ec.Op, body)

	case 429:
		return fmt.Sprintf("Cannot %s: Nomad is rate limiting requests. Retry in a moment.", ec.Op)

	case 500, 502, 503, 504:
		return fmt.Sprintf(
			"Cannot %s: Nomad returned a server error (HTTP %d). The cluster may be unhealthy or without a leader; try get_cluster_status. %s",
			ec.Op, code, body)
	}

	return fmt.Sprintf("Failed to %s: Nomad returned HTTP %d. %s", ec.Op, code, body)
}

// forbiddenMessage explains a 403. Nomad's body is always "Permission denied",
// so everything useful here comes from ErrorContext.
func forbiddenMessage(ec ErrorContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cannot %s: your NOMAD_TOKEN lacks the required permission", ec.Op)

	if ec.Capability != "" {
		fmt.Fprintf(&b, " (capability %q", ec.Capability)
		if ec.Namespace != "" {
			fmt.Fprintf(&b, " in namespace %q", ec.Namespace)
		}
		b.WriteString(")")
	} else if ec.Namespace != "" {
		fmt.Fprintf(&b, " in namespace %q", ec.Namespace)
	}

	b.WriteString(". Nomad does not report which capability was missing, so this is the capability the endpoint documents.")
	b.WriteString(" Check the policy attached to your token, or whether the token is set at all.")
	return b.String()
}

// notFoundMessage explains a 404 and points at the tool that lists valid names.
func notFoundMessage(ec ErrorContext, body string) string {
	kind := ec.Kind
	if kind == "" {
		kind = "resource"
	}

	var b strings.Builder
	if ec.Name != "" {
		fmt.Fprintf(&b, "No %s named %q", kind, ec.Name)
	} else {
		fmt.Fprintf(&b, "No such %s", kind)
	}
	if ec.Namespace != "" {
		fmt.Fprintf(&b, " in namespace %q", ec.Namespace)
	}
	b.WriteString(".")

	if ec.ListTool != "" {
		fmt.Fprintf(&b, " Try %s to see what exists", ec.ListTool)
		if ec.Namespace != "" {
			b.WriteString(", or check whether it lives in a different namespace")
		}
		b.WriteString(".")
	}

	// Nomad's 404 bodies are terse but occasionally more specific than the
	// generic "<kind> not found"; surface anything that adds information.
	if body != "" && !strings.EqualFold(body, kind+" not found") &&
		!strings.EqualFold(body, "not found") {
		fmt.Fprintf(&b, " Nomad said: %s", body)
	}

	return b.String()
}

// mapConnectionError handles the failures that happen before Nomad ever
// answers: nothing listening, DNS failure, timeout, or a TLS problem.
func mapConnectionError(err error, ec ErrorContext) (string, bool) {
	addr := ec.addressOrDefault()

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Sprintf(
			"Cannot reach the Nomad API at %s: the hostname could not be resolved. Check NOMAD_ADDR.",
			addr), true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf(
			"Cannot reach the Nomad API at %s: the request timed out. The agent may be unreachable or overloaded.",
			addr), true
	}

	// Fall back to string matching. The Nomad client wraps transport errors in
	// its own types, so the sentinel errors are not always recoverable.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Sprintf(
			"Cannot reach the Nomad API at %s: connection refused. Is the Nomad agent running, and is NOMAD_ADDR correct?",
			addr), true

	case strings.Contains(msg, "no such host"):
		return fmt.Sprintf(
			"Cannot reach the Nomad API at %s: the hostname could not be resolved. Check NOMAD_ADDR.",
			addr), true

	case strings.Contains(msg, "certificate") || strings.Contains(msg, "x509") ||
		strings.Contains(msg, "tls: "):
		return fmt.Sprintf(
			"Cannot reach the Nomad API at %s: TLS verification failed. Check NOMAD_CACERT, NOMAD_CAPATH and NOMAD_TLS_SERVER_NAME. %s",
			addr, err.Error()), true

	case strings.Contains(msg, "connection reset"):
		return fmt.Sprintf(
			"Cannot reach the Nomad API at %s: the connection was reset. If the agent uses TLS, NOMAD_ADDR must use https://.",
			addr), true
	}

	// A URL error that got this far is still a transport problem.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprintf("Cannot reach the Nomad API at %s: %s", addr, urlErr.Err.Error()), true
	}

	return "", false
}

// StatusCode extracts the HTTP status and response body from a Nomad API error.
//
// The typed api.UnexpectedResponseError is preferred. Some code paths in the
// Nomad client format the status into a plain error string instead, so there is
// a textual fallback for "Unexpected response code: 404 (job not found)".
func StatusCode(err error) (code int, body string, ok bool) {
	if err == nil {
		return 0, "", false
	}

	var ure api.UnexpectedResponseError
	if errors.As(err, &ure) {
		return ure.StatusCode(), ure.Body(), true
	}

	return parseUnexpectedResponse(err.Error())
}

// parseUnexpectedResponse recovers the status code and body from the string
// form the Nomad client sometimes produces.
func parseUnexpectedResponse(msg string) (int, string, bool) {
	const marker = "Unexpected response code: "
	i := strings.Index(msg, marker)
	if i < 0 {
		return 0, "", false
	}
	rest := msg[i+len(marker):]

	// The code is the leading run of digits.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, "", false
	}

	code := 0
	for _, c := range rest[:end] {
		code = code*10 + int(c-'0')
	}

	body := strings.TrimSpace(rest[end:])
	body = strings.TrimPrefix(body, "(")
	body = strings.TrimSuffix(body, ")")

	return code, strings.TrimSpace(body), true
}

// IsNotFound reports whether err is a Nomad 404. Useful for tools that treat a
// missing resource as an empty result rather than an error.
func IsNotFound(err error) bool {
	code, _, ok := StatusCode(err)
	return ok && code == 404
}

// IsForbidden reports whether err is a Nomad 403.
func IsForbidden(err error) bool {
	code, _, ok := StatusCode(err)
	return ok && code == 403
}

// IsEnterpriseOnly reports whether err came from an Enterprise-only endpoint on
// a Community Edition cluster.
func IsEnterpriseOnly(err error) bool {
	code, body, ok := StatusCode(err)
	if !ok {
		return false
	}
	return code == 501 || strings.Contains(strings.ToLower(body), enterpriseMarker)
}

// addressOrDefault returns the address to name in a message.
func (ec ErrorContext) addressOrDefault() string {
	if ec.Address != "" {
		return ec.Address
	}
	return "the configured NOMAD_ADDR"
}
