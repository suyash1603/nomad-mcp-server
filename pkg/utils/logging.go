// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package utils

import (
	"context"
	"log/slog"

	log "github.com/sirupsen/logrus"
)

// SlogFromLogrus adapts a logrus logger to the *slog.Logger that mcp-go's
// server options expect.
//
// The server has one logger, configured once from --log-file and --log-level.
// mcp-go wants slog; everything else in this codebase uses logrus. Rather than
// running two independent log streams — which in stdio mode risks one of them
// finding its way to stdout and corrupting the JSON-RPC channel — this forwards
// slog records into the same logrus logger.
func SlogFromLogrus(logger *log.Logger) *slog.Logger {
	return slog.New(&logrusHandler{logger: logger})
}

// logrusHandler is a slog.Handler that writes through to logrus.
type logrusHandler struct {
	logger *log.Logger
	attrs  []slog.Attr
	groups []string
}

func (h *logrusHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.logger.IsLevelEnabled(toLogrusLevel(level))
}

func (h *logrusHandler) Handle(_ context.Context, r slog.Record) error {
	fields := make(log.Fields, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		fields[h.qualify(a.Key)] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		fields[h.qualify(a.Key)] = a.Value.Any()
		return true
	})

	h.logger.WithFields(fields).Log(toLogrusLevel(r.Level), r.Message)
	return nil
}

func (h *logrusHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := h.clone()
	next.attrs = append(next.attrs, attrs...)
	return next
}

func (h *logrusHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h.clone()
	next.groups = append(next.groups, name)
	return next
}

func (h *logrusHandler) clone() *logrusHandler {
	return &logrusHandler{
		logger: h.logger,
		attrs:  append([]slog.Attr(nil), h.attrs...),
		groups: append([]string(nil), h.groups...),
	}
}

// qualify prefixes a key with any open groups, so grouped attributes do not
// collide once flattened into logrus fields.
func (h *logrusHandler) qualify(key string) string {
	if len(h.groups) == 0 {
		return key
	}
	out := ""
	for _, g := range h.groups {
		out += g + "."
	}
	return out + key
}

// toLogrusLevel maps slog levels onto logrus levels. slog's scale is numeric
// and open-ended, so the comparisons are ordered rather than exact matches.
func toLogrusLevel(l slog.Level) log.Level {
	switch {
	case l < slog.LevelDebug:
		return log.TraceLevel
	case l < slog.LevelInfo:
		return log.DebugLevel
	case l < slog.LevelWarn:
		return log.InfoLevel
	case l < slog.LevelError:
		return log.WarnLevel
	default:
		return log.ErrorLevel
	}
}
