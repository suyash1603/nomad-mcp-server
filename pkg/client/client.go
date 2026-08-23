// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

// Package client owns the connection to Nomad: how the API client is built,
// how one is kept per MCP session, and the middleware that wraps both the HTTP
// transport and every tool call.
package client

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"

	"github.com/suyash1603/nomad-mcp-server/pkg/config"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// Header names accepted on the HTTP transport. NomadTokenHeader matches the
// header the Nomad API itself uses, so an operator proxying to this server can
// forward the same header they already send to Nomad.
const (
	NomadTokenHeader     = "X-Nomad-Token"
	NomadNamespaceHeader = "X-Nomad-Namespace"
	NomadRegionHeader    = "X-Nomad-Region"
)

// contextKey is a private type so context values set here cannot collide with
// any other package's keys.
type contextKey string

const (
	ctxKeyToken     contextKey = "nomad-token"
	ctxKeyNamespace contextKey = "nomad-namespace"
	ctxKeyRegion    contextKey = "nomad-region"
)

// sessionEntry caches a Nomad client alongside a hash of the token it was built
// with. Both are required to reuse it: a session ID alone is not a credential,
// and treating it as one would let a leaked or guessed ID inherit another
// session's token.
type sessionEntry struct {
	client    *api.Client
	tokenHash [32]byte
}

// Provider hands out Nomad clients to tool handlers, and carries the config,
// logger and redactor that every tool needs.
//
// One Provider is built at startup and shared. In stdio mode there is a single
// session and effectively a single client. In HTTP mode there is one client per
// MCP session, so two clients connecting with different tokens stay isolated.
type Provider struct {
	cfg      *config.Config
	logger   *log.Logger
	redactor *utils.Redactor

	sessions sync.Map // sessionID -> *sessionEntry

	// edition caches what build of Nomad the cluster runs, so that the dozen
	// Enterprise-only tools can explain themselves without every one of them
	// re-probing.
	edition editionCache

	// fallback serves calls that arrive without an MCP session, which happens
	// in tests and in direct library use.
	fallback *api.Client
}

// New builds a Provider and eagerly constructs a client from the configured
// settings, so that a malformed address or an unreadable CA file is reported at
// startup rather than on the first tool call.
func New(cfg *config.Config, logger *log.Logger) (*Provider, error) {
	redactor := utils.NewRedactor(cfg.NomadToken)

	fallback, err := buildClient(cfg, cfg.NomadToken)
	if err != nil {
		return nil, fmt.Errorf("failed to build Nomad client: %s", redactor.Error(err))
	}

	return &Provider{
		cfg:      cfg,
		logger:   logger,
		redactor: redactor,
		fallback: fallback,
	}, nil
}

// Config returns the server configuration.
func (p *Provider) Config() *config.Config { return p.cfg }

// Logger returns the shared logger.
func (p *Provider) Logger() *log.Logger { return p.logger }

// Redactor returns the shared redactor.
func (p *Provider) Redactor() *utils.Redactor { return p.redactor }

// Address returns the configured Nomad address, for error messages.
func (p *Provider) Address() string { return p.cfg.NomadAddr }

// buildClient constructs a Nomad API client for one token.
//
// It starts from api.DefaultConfig() because that already understands every
// NOMAD_* environment variable, then overrides with the resolved configuration
// so that a flag still beats the environment. TLSConfig is assigned rather than
// mutated in place: api.NewClient calls api.ConfigureTLS itself whenever
// HttpClient is nil, so leaving HttpClient unset is what gets the CA file,
// client certificate and SNI name wired up.
func buildClient(cfg *config.Config, token string) (*api.Client, error) {
	c := api.DefaultConfig()

	c.Address = cfg.NomadAddr
	c.Region = cfg.NomadRegion
	c.Namespace = cfg.NomadNamespace
	c.SecretID = token

	c.TLSConfig = &api.TLSConfig{
		CACert:        cfg.NomadCACert,
		CAPath:        cfg.NomadCAPath,
		ClientCert:    cfg.NomadClientCert,
		ClientKey:     cfg.NomadClientKey,
		TLSServerName: cfg.NomadTLSServerName,
		Insecure:      cfg.NomadSkipVerify,
	}

	return api.NewClient(c)
}

// FromContext returns the Nomad client for the calling MCP session, building
// one if this session has not been seen or if its token has changed.
func (p *Provider) FromContext(ctx context.Context) (*api.Client, error) {
	session := server.ClientSessionFromContext(ctx)
	if session == nil {
		// No session: stdio before initialization, tests, or direct use.
		return p.fallback, nil
	}

	id := session.SessionID()
	token := p.resolveToken(ctx)

	if entry, ok := p.load(id); ok {
		want := hashToken(token)
		if subtle.ConstantTimeCompare(entry.tokenHash[:], want[:]) == 1 {
			return entry.client, nil
		}
		// The token changed mid-session. Rebuild rather than reuse: the cached
		// client still carries the old credential.
		p.logger.WithField("session_id", id).Info("Nomad token for session changed; rebuilding client")
	}

	return p.clientFor(id, token)
}

// clientFor builds and caches a client for one session.
func (p *Provider) clientFor(sessionID, token string) (*api.Client, error) {
	c, err := buildClient(p.cfg, token)
	if err != nil {
		return nil, fmt.Errorf("failed to build Nomad client: %s", p.redactor.Error(err))
	}

	p.sessions.Store(sessionID, &sessionEntry{client: c, tokenHash: hashToken(token)})

	p.logger.WithFields(log.Fields{
		"session_id": sessionID,
		"nomad_addr": p.cfg.NomadAddr,
		"token_set":  token != "",
	}).Debug("built Nomad client for session")

	return c, nil
}

func (p *Provider) load(sessionID string) (*sessionEntry, bool) {
	v, ok := p.sessions.Load(sessionID)
	if !ok {
		return nil, false
	}
	entry, ok := v.(*sessionEntry)
	return entry, ok
}

// resolveToken picks the token for this request: a per-request token from the
// HTTP transport if one was supplied, otherwise the server's configured token.
func (p *Provider) resolveToken(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyToken).(string); ok && v != "" {
		return v
	}
	return p.cfg.NomadToken
}

// ResolveNamespace determines which namespace a tool call should target and
// enforces the allowlist.
//
// Precedence is the tool's explicit argument, then a per-request namespace from
// the HTTP transport, then the server default. The allowlist is checked last,
// and checked here rather than at the API — refusing before the call means a
// disallowed namespace never reaches Nomad and never appears in its audit log.
func (p *Provider) ResolveNamespace(ctx context.Context, requested string) (string, error) {
	ns := strings.TrimSpace(requested)

	if ns == "" {
		if v, ok := ctx.Value(ctxKeyNamespace).(string); ok && v != "" {
			ns = v
		}
	}
	if ns == "" {
		ns = p.cfg.NomadNamespace
	}
	if ns == "" {
		ns = config.DefaultNomadNamespace
	}

	// "*" means "all namespaces" to Nomad's list endpoints. Permit it only
	// when no allowlist is configured, since it would otherwise be a trivial
	// way around the allowlist.
	if ns == "*" && len(p.cfg.AllowedNamespaces) > 0 {
		return "", fmt.Errorf(
			"namespace %q is not permitted: this server is restricted to the namespaces [%s], so it cannot query all namespaces at once. Name one of them explicitly",
			ns, strings.Join(p.cfg.AllowedNamespaces, ", "))
	}

	if !p.cfg.NamespaceAllowed(ns) {
		return "", fmt.Errorf(
			"namespace %q is not permitted by this server's configuration. Allowed namespaces: [%s]. This is enforced by NOMAD_MCP_ALLOWED_NAMESPACES, not by your Nomad token",
			ns, strings.Join(p.cfg.AllowedNamespaces, ", "))
	}

	return ns, nil
}

// ResolveRegion returns the region for a call: the tool's argument, then a
// per-request region, then the configured default. An empty result means "use
// the agent's own region", which is what the Nomad API expects.
func (p *Provider) ResolveRegion(ctx context.Context, requested string) string {
	if r := strings.TrimSpace(requested); r != "" {
		return r
	}
	if v, ok := ctx.Value(ctxKeyRegion).(string); ok && v != "" {
		return v
	}
	return p.cfg.NomadRegion
}

// EndSession drops the cached client for a finished session.
func (p *Provider) EndSession(sessionID string) {
	if _, ok := p.sessions.LoadAndDelete(sessionID); ok {
		p.logger.WithField("session_id", sessionID).Debug("released Nomad client for session")
	}
}

// RegisterHooks wires session lifecycle callbacks so clients are cleaned up
// when a session ends, rather than accumulating for the process's lifetime.
func (p *Provider) RegisterHooks(hooks *server.Hooks) {
	hooks.AddOnUnregisterSession(func(_ context.Context, session server.ClientSession) {
		p.EndSession(session.SessionID())
	})
}

// hashToken returns a SHA-256 of a token. Only the hash is retained, so a heap
// dump of the session cache does not yield usable credentials.
func hashToken(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// WithToken returns a context carrying a per-request Nomad token. Used by the
// HTTP middleware, and by tests.
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, ctxKeyToken, token)
}

// WithNamespace returns a context carrying a per-request namespace.
func WithNamespace(ctx context.Context, ns string) context.Context {
	return context.WithValue(ctx, ctxKeyNamespace, ns)
}

// WithRegion returns a context carrying a per-request region.
func WithRegion(ctx context.Context, region string) context.Context {
	return context.WithValue(ctx, ctxKeyRegion, region)
}

// NomadEnv returns the NOMAD_* environment variables a child process needs to
// talk to the same cluster, with the same credentials, as this session.
//
// It exists so that the token never has to leave this package as a bare string.
// The one tool that shells out — collect_hcdiag — needs hcdiag to reach Nomad,
// and hcdiag reads the standard NOMAD_* variables, so handing it an environment
// is both the natural interface and the safe one: values passed this way are
// not visible in `ps`, which is exactly why NOMAD_TOKEN has no command-line
// flag on this server either.
//
// Only variables that are actually set are returned, so a child inherits
// nothing this server was not itself configured with.
func (p *Provider) NomadEnv(ctx context.Context) []string {
	pairs := []struct{ key, value string }{
		{"NOMAD_ADDR", p.cfg.NomadAddr},
		{"NOMAD_TOKEN", p.resolveToken(ctx)},
		{"NOMAD_REGION", p.cfg.NomadRegion},
		{"NOMAD_NAMESPACE", p.cfg.NomadNamespace},
		{"NOMAD_CACERT", p.cfg.NomadCACert},
		{"NOMAD_CAPATH", p.cfg.NomadCAPath},
		{"NOMAD_CLIENT_CERT", p.cfg.NomadClientCert},
		{"NOMAD_CLIENT_KEY", p.cfg.NomadClientKey},
		{"NOMAD_TLS_SERVER_NAME", p.cfg.NomadTLSServerName},
	}

	env := make([]string, 0, len(pairs)+1)
	for _, pair := range pairs {
		if pair.value != "" {
			env = append(env, pair.key+"="+pair.value)
		}
	}
	if p.cfg.NomadSkipVerify {
		env = append(env, "NOMAD_SKIP_VERIFY=true")
	}
	return env
}
