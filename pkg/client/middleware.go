// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"net/http"
	"net/textproto"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

// CORSConfig holds the resolved CORS policy.
type CORSConfig struct {
	AllowedOrigins []string
	Mode           string // strict, development, or disabled
}

// LoadCORSConfig reads the CORS policy from the server configuration.
func LoadCORSConfig(cfg *config.Config) CORSConfig {
	mode := cfg.MCPCORSMode
	if mode == "" {
		mode = config.DefaultCORSMode
	}
	return CORSConfig{AllowedOrigins: cfg.MCPAllowedOrigins, Mode: mode}
}

// isOriginAllowed applies the CORS policy to one Origin header.
func isOriginAllowed(origin string, allowed []string, mode string) bool {
	if mode == "disabled" {
		return true
	}

	for _, a := range allowed {
		if origin == a {
			return true
		}
	}

	// Development mode additionally trusts loopback origins, which is what a
	// locally running MCP Inspector or web client sends.
	if mode == "development" {
		for _, prefix := range []string{
			"http://localhost:", "https://localhost:",
			"http://127.0.0.1:", "https://127.0.0.1:",
			"http://[::1]:", "https://[::1]:",
		} {
			if strings.HasPrefix(origin, prefix) {
				return true
			}
		}
	}

	return false
}

// securityHandler validates the Origin header before the MCP handler sees a
// request.
//
// This is not decoration. An MCP server on localhost holding a Nomad token is
// reachable by any page the user visits, so without origin validation a
// malicious site could drive this server through the browser — the DNS
// rebinding class of attack. Strict mode, the default, rejects every
// cross-origin request outright.
type securityHandler struct {
	handler http.Handler
	cors    CORSConfig
	logger  *log.Logger
}

// NewSecurityHandler wraps h with Origin validation and CORS headers.
func NewSecurityHandler(h http.Handler, cors CORSConfig, logger *log.Logger) http.Handler {
	return &securityHandler{handler: h, cors: cors, logger: logger}
}

func (h *securityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")

	if origin != "" {
		if !isOriginAllowed(origin, h.cors.AllowedOrigins, h.cors.Mode) {
			h.logger.WithFields(log.Fields{
				"origin":    origin,
				"cors_mode": h.cors.Mode,
			}).Warn("rejected request from unauthorized origin")
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}

		h.logger.WithField("origin", origin).Debug("allowed cross-origin request")

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers",
			strings.Join([]string{
				"Content-Type", "Authorization", "Accept",
				"Mcp-Session-Id", "MCP-Protocol-Version",
				NomadTokenHeader, NomadNamespaceHeader, NomadRegionHeader,
			}, ", "))
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
		w.Header().Set("Access-Control-Max-Age", "3600")
		// Responses differ by Origin, so caches must not share them.
		w.Header().Add("Vary", "Origin")
	}

	if r.Method == http.MethodOptions {
		h.logger.WithField("origin", origin).Debug("handling CORS preflight")
		w.WriteHeader(http.StatusOK)
		return
	}

	h.handler.ServeHTTP(w, r)
}

// NomadContextMiddleware lifts per-request Nomad settings out of HTTP headers
// and into the request context, where Provider can find them.
//
// Headers only — never query parameters. A token in a query string ends up in
// proxy logs, browser history and Referer headers, and an address in a query
// string turns this server into an SSRF gadget that will happily send the
// caller's token to a host of their choosing. Both are rejected loudly rather
// than ignored, so a client using them finds out immediately.
func NomadContextMiddleware(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()

			for _, forbidden := range []string{
				NomadTokenHeader, config.EnvNomadToken,
				"token", "secret_id", "secretid",
			} {
				if q.Get(forbidden) != "" {
					logger.WithField("remote_ip", r.RemoteAddr).
						Warn("rejected request carrying a Nomad token in the query string")
					http.Error(w,
						"A Nomad token must not be passed as a query parameter; use the "+
							NomadTokenHeader+" header.", http.StatusBadRequest)
					return
				}
			}

			for _, forbidden := range []string{config.EnvNomadAddr, "nomad_addr", "address"} {
				if q.Get(forbidden) != "" {
					logger.WithField("remote_ip", r.RemoteAddr).
						Warn("rejected request overriding the Nomad address via the query string")
					http.Error(w,
						"The Nomad address must not be passed as a query parameter.",
						http.StatusBadRequest)
					return
				}
			}

			ctx := r.Context()

			if token := header(r, NomadTokenHeader); token != "" {
				ctx = WithToken(ctx, token)
				logger.Debug("Nomad token supplied via request header")
			}
			if ns := header(r, NomadNamespaceHeader); ns != "" {
				ctx = WithNamespace(ctx, ns)
				logger.WithField("namespace", ns).Debug("Nomad namespace supplied via request header")
			}
			if region := header(r, NomadRegionHeader); region != "" {
				ctx = WithRegion(ctx, region)
				logger.WithField("region", region).Debug("Nomad region supplied via request header")
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// header reads a header case-insensitively.
func header(r *http.Request, name string) string {
	return r.Header.Get(textproto.CanonicalMIMEHeaderKey(name))
}

// LoggingMiddleware records each HTTP request with its status and duration.
// It never logs headers or bodies, both of which carry tokens.
func LoggingMiddleware(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			logger.WithFields(log.Fields{
				"method":      r.Method,
				"path":        r.URL.Path,
				"status":      rec.status,
				"duration_ms": time.Since(start).Milliseconds(),
				"remote_ip":   r.RemoteAddr,
				"user_agent":  r.UserAgent(),
			}).Info("HTTP request")
		})
	}
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped writer so SSE streaming keeps working through
// the access log wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
