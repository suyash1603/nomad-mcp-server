// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
)

// RateLimitConfig holds the two limits the server enforces.
type RateLimitConfig struct {
	GlobalLimit     rate.Limit
	GlobalBurst     int
	PerSessionLimit rate.Limit
	PerSessionBurst int
}

// DefaultRateLimitConfig matches the documented defaults, 10:20 and 5:10.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		GlobalLimit:     rate.Limit(10),
		GlobalBurst:     20,
		PerSessionLimit: rate.Limit(5),
		PerSessionBurst: 10,
	}
}

// ParseRateLimits builds a RateLimitConfig from the two "rps:burst" strings.
// Values are validated at startup by config.Validate, so a parse failure here
// falls back to the default rather than failing the request.
func ParseRateLimits(cfg *config.Config, logger *log.Logger) RateLimitConfig {
	out := DefaultRateLimitConfig()

	if rps, burst, ok := parseRateLimit(cfg.RateLimitGlobal); ok {
		out.GlobalLimit, out.GlobalBurst = rate.Limit(rps), burst
	} else if cfg.RateLimitGlobal != "" {
		logger.Warnf("invalid %s %q; using default", config.EnvMCPRateLimitGlob, cfg.RateLimitGlobal)
	}

	if rps, burst, ok := parseRateLimit(cfg.RateLimitSession); ok {
		out.PerSessionLimit, out.PerSessionBurst = rate.Limit(rps), burst
	} else if cfg.RateLimitSession != "" {
		logger.Warnf("invalid %s %q; using default", config.EnvMCPRateLimitSess, cfg.RateLimitSession)
	}

	return out
}

// parseRateLimit parses the "rps:burst" form.
func parseRateLimit(s string) (float64, int, bool) {
	rpsStr, burstStr, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	rps, err1 := strconv.ParseFloat(strings.TrimSpace(rpsStr), 64)
	burst, err2 := strconv.Atoi(strings.TrimSpace(burstStr))
	if err1 != nil || err2 != nil || rps <= 0 || burst <= 0 {
		return 0, 0, false
	}
	return rps, burst, true
}

// RateLimiter throttles tool calls, globally and per MCP session.
//
// The two limits do different jobs. The global limit protects the Nomad cluster
// from this server as a whole. The per-session limit stops one runaway client —
// a model stuck in a retry loop, most often — from consuming the entire global
// budget and starving everyone else.
type RateLimiter struct {
	cfg    RateLimitConfig
	logger *log.Logger

	global   *rate.Limiter
	sessions sync.Map // sessionID -> *rate.Limiter
}

// NewRateLimiter builds a RateLimiter.
func NewRateLimiter(cfg RateLimitConfig, logger *log.Logger) *RateLimiter {
	return &RateLimiter{
		cfg:    cfg,
		logger: logger,
		global: rate.NewLimiter(cfg.GlobalLimit, cfg.GlobalBurst),
	}
}

// Middleware returns tool handler middleware that enforces both limits.
func (r *RateLimiter) Middleware() server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !r.global.Allow() {
				r.logger.WithField("tool", req.Params.Name).
					Warn("global rate limit exceeded")
				return mcp.NewToolResultError(rateLimitMessage("server-wide")), nil
			}

			if session := server.ClientSessionFromContext(ctx); session != nil {
				if !r.sessionLimiter(session.SessionID()).Allow() {
					r.logger.WithFields(log.Fields{
						"tool":       req.Params.Name,
						"session_id": session.SessionID(),
					}).Warn("per-session rate limit exceeded")
					return mcp.NewToolResultError(rateLimitMessage("for this session")), nil
				}
			}

			return next(ctx, req)
		}
	}
}

// sessionLimiter returns the limiter for a session, creating it on first use.
func (r *RateLimiter) sessionLimiter(sessionID string) *rate.Limiter {
	if v, ok := r.sessions.Load(sessionID); ok {
		return v.(*rate.Limiter)
	}
	limiter := rate.NewLimiter(r.cfg.PerSessionLimit, r.cfg.PerSessionBurst)
	actual, _ := r.sessions.LoadOrStore(sessionID, limiter)
	return actual.(*rate.Limiter)
}

// EndSession drops a session's limiter.
func (r *RateLimiter) EndSession(sessionID string) {
	r.sessions.Delete(sessionID)
}

// RegisterHooks wires session cleanup.
func (r *RateLimiter) RegisterHooks(hooks *server.Hooks) {
	hooks.AddOnUnregisterSession(func(_ context.Context, session server.ClientSession) {
		r.EndSession(session.SessionID())
	})
}

// rateLimitMessage tells the model to slow down rather than retry immediately,
// which is the behaviour that caused the limit to trip in the first place.
func rateLimitMessage(scope string) string {
	return fmt.Sprintf(
		"Rate limit exceeded %s. Wait a moment before trying again, and avoid retrying in a tight loop. "+
			"If you were polling for a change, prefer a single call after a short pause.", scope)
}
