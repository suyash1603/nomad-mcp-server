// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"context"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// tokenSummary is the list projection.
//
// There is no secret field, and that is a property of Nomad's list endpoint
// rather than a filter applied here: /v1/acl/tokens returns stubs that have no
// SecretID at all. Listing tokens therefore discloses nothing usable, which is
// why this tool is not gated beyond the toolset switch itself.
type tokenSummary struct {
	AccessorID string   `json:"accessor_id"`
	Name       string   `json:"name,omitempty"`
	Type       string   `json:"type"`
	Policies   []string `json:"policies,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Global     bool     `json:"global"`
	Created    string   `json:"created,omitempty"`
	Expires    string   `json:"expires,omitempty"`
	Expired    bool     `json:"expired,omitempty"`
}

// ListACLTokens lists the tokens that exist, without any secret.
func ListACLTokens(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List the ACL tokens that exist in this cluster: who they are for, what type they " +
				"are, and which policies and roles each one carries.\n\n" +
				"This returns ACCESSOR IDs and never returns any token's secret. That is a " +
				"property of Nomad's list endpoint, not a filter applied afterwards — the " +
				"accessor ID is a handle for managing a token and cannot authenticate as one.\n\n" +
				"Use it to audit who holds access, to find the accessor ID of a token someone is " +
				"having trouble with, or to spot management tokens — which ignore every policy " +
				"and are worth knowing the count of. Expired tokens are flagged, because an " +
				"expired token is a common and confusing cause of \"Permission denied\".\n\n" +
				"Requires the acl:read capability, which in practice means a management token."),
		utils.ReadOnlyTool(),
		utils.RegionParam(),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_acl_tokens", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !enabled(p) {
				return utils.ErrorResult(disabledMessage("list_acl_tokens"))
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))
			q := utils.PageFrom(req).Apply(&api.QueryOptions{Region: region})

			tokens, meta, err := nomad.ACLTokens().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list ACL tokens",
					Kind:       "ACL token",
					Address:    p.Address(),
					Capability: "acl:read",
				}, p.Redactor()))
			}

			var management, expired int
			items := make([]tokenSummary, 0, len(tokens))
			for _, tok := range tokens {
				if tok == nil {
					continue
				}
				expires, isExpired := formatExpiry(tok.ExpirationTime)
				if tok.Type == TypeManagement {
					management++
				}
				if isExpired {
					expired++
				}
				items = append(items, tokenSummary{
					AccessorID: tok.AccessorID,
					Name:       tok.Name,
					Type:       tok.Type,
					Policies:   tok.Policies,
					Roles:      roleLinkNames(tok.Roles),
					Global:     tok.Global,
					Created:    utils.FormatTime(tok.CreateTime.UnixNano()),
					Expires:    expires,
					Expired:    isExpired,
				})
			}

			result := utils.List{Count: len(items), Region: region, Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}

			// The two counts worth stating in words. A page of tokens where a
			// third are management tokens is a finding; reading that off the
			// type field of every row is work the reader should not have to do.
			var extra []string
			if management > 0 {
				extra = append(extra, itoa(management)+" of these are management tokens, which "+
					"carry every capability in the cluster and ignore policies entirely.")
			}
			if expired > 0 {
				extra = append(extra, itoa(expired)+" have already expired and no longer "+
					"authenticate, though Nomad still lists them until they are garbage collected.")
			}
			if len(extra) > 0 {
				result.Note = strings.TrimSpace(result.Note + " " + strings.Join(extra, " "))
			}
			if result.Note == "" && len(items) == 0 {
				result.Note = "No ACL tokens were returned. On a cluster with ACLs enabled this " +
					"usually means the token in use cannot read the ACL system rather than that " +
					"no tokens exist."
			}
			return utils.JSONResult(result)
		},
	}
}

// ReadACLToken reads one token by accessor ID.
func ReadACLToken(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_acl_token",
			mcp.WithDescription(
				"Read one ACL token by its accessor ID: its type, the policies and roles it "+
					"carries, whether it is global, and when it was created and expires.\n\n"+
					"Use this to answer \"what is this token allowed to do\" — then read the "+
					"policies it names with read_acl_policy to see the actual grants. Together "+
					"those two calls explain almost every \"Permission denied\" anyone brings you.\n\n"+
					"The token's SECRET is withheld by default even though Nomad returns it here. "+
					"The secret is the credential itself, and putting one into this conversation "+
					"discloses it to wherever this conversation goes, permanently. The accessor ID "+
					"is enough to manage the token; the operator can retrieve the secret with "+
					"`nomad acl token info <accessor_id>` at a terminal. Returning it requires the "+
					"operator to have set NOMAD_MCP_ALLOW_TOKEN_SECRETS=true.\n\n"+
					"Requires the acl:read capability."),
			utils.ReadOnlyTool(),
			mcp.WithString("accessor_id",
				mcp.Required(),
				mcp.Description(
					"The token's accessor ID, as returned by list_acl_tokens. This is the "+
						"management handle, not the secret."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !enabled(p) {
				return utils.ErrorResult(disabledMessage("read_acl_token"))
			}
			accessor, err := req.RequireString("accessor_id")
			if err != nil {
				return utils.ErrorResult(
					"The 'accessor_id' argument is required. Use list_acl_tokens to see what exists.")
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			tok, _, err := nomad.ACLTokens().Info(accessor, &api.QueryOptions{
				Region: p.ResolveRegion(ctx, req.GetString("region", "")),
			})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "read ACL token " + utils.ShortID(accessor),
					Kind:       "ACL token",
					Name:       accessor,
					Address:    p.Address(),
					Capability: "acl:read",
					ListTool:   "list_acl_tokens",
				}, p.Redactor()))
			}
			if tok == nil || tok.AccessorID == "" {
				return utils.ErrorResultf(
					"No ACL token with accessor ID %q exists. Use list_acl_tokens to see what does.",
					accessor)
			}

			return utils.JSONResult(tokenProjection(p, tok, nil))
		},
	}
}

// CreateACLToken mints a new token.
func CreateACLToken(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("create_acl_token",
			mcp.WithDescription(
				"Create a new ACL token, granting whoever holds it the capabilities of the "+
					"policies and roles it names.\n\n"+
					"This mints a credential. It is not a change you can take back by calling this "+
					"again: once the secret exists it is valid until somebody explicitly revokes "+
					"it, and anything that has seen it keeps working. Confirm the exact name, "+
					"type, policies and expiry with the user before calling this, and do not "+
					"create a token because it seemed like the obvious way to fix an access "+
					"problem — fixing the existing token's policies is almost always the better "+
					"answer.\n\n"+
					"Set an expiration_ttl whenever the token is for a person, a debugging "+
					"session or anything temporary. An unexpiring token is forever by default, "+
					"and forever is how clusters accumulate credentials nobody can account for.\n\n"+
					"type=\"management\" is not a stronger client token: it grants EVERY "+
					"capability in the cluster, ignores policies entirely, and can create further "+
					"management tokens. Do not create one unless the user has asked for a "+
					"management token in those words.\n\n"+
					"The new token's SECRET is withheld from the response by default. The accessor "+
					"ID is returned instead, and the operator retrieves the secret with "+
					"`nomad acl token info <accessor_id>` at a terminal — so nothing is lost by "+
					"the secret never entering this conversation. Returning it requires the "+
					"operator to have set NOMAD_MCP_ALLOW_TOKEN_SECRETS=true.\n\n"+
					"Requires the acl:write capability, which in practice means a management token."),
			// Destructive, though it discards nothing: the destructive tier is
			// this server's "nothing irreversible" line, and minting a
			// credential is irreversible in the way that matters. A secret that
			// has existed cannot be un-issued, only revoked, and revocation
			// does not reach whatever already copied it.
			//
			// Not idempotent: every call mints a distinct token, so a retry
			// after an ambiguous failure leaves two.
			utils.MutatingTool(true, false),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description(
					"A human-readable name identifying who or what this token is for. Nomad does "+
						"not require it to be unique, so make it specific enough that someone "+
						"auditing the token list in six months knows what it was issued for."),
			),
			mcp.WithString("type",
				mcp.DefaultString(TypeClient),
				mcp.Enum(TypeClient, TypeManagement),
				mcp.Description(typeDescription),
			),
			mcp.WithArray("policies",
				mcp.WithStringItems(),
				mcp.Description(
					"Names of the ACL policies this token carries, from list_acl_policies. "+
						"Required for a client token unless roles are given. Must be empty for a "+
						"management token, which ignores policies."),
			),
			mcp.WithArray("roles",
				mcp.WithStringItems(),
				mcp.Description(
					"Names of the ACL roles this token carries, from list_acl_roles. A role is a "+
						"named bundle of policies, and is the better choice when several tokens "+
						"should share one set of grants."),
			),
			mcp.WithString("expiration_ttl",
				mcp.Description(
					"How long the token stays valid, as a Go duration such as \"8h\", \"720h\" or "+
						"\"30m\". Omit it and the token never expires. Set it for anything "+
						"temporary — Nomad cannot add an expiry to a token afterwards."),
			),
			mcp.WithBoolean("global",
				mcp.DefaultBool(false),
				mcp.Description(
					"Replicate the token to every federated region rather than keeping it local "+
						"to this one. Only meaningful on a multi-region cluster; a global token "+
						"is a credential in every region at once."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return createToken(ctx, req, p)
		},
	}
}

func createToken(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	if !enabled(p) {
		return utils.ErrorResult(disabledMessage("create_acl_token"))
	}
	name, err := req.RequireString("name")
	if err != nil {
		return utils.ErrorResult(
			"The 'name' argument is required: a human-readable name saying who or what this " +
				"token is for.")
	}
	if strings.TrimSpace(name) == "" {
		return utils.ErrorResult("The 'name' argument cannot be empty.")
	}

	typ := strings.ToLower(strings.TrimSpace(req.GetString("type", TypeClient)))
	policies := utils.StringSlice(req, "policies")
	roles := utils.StringSlice(req, "roles")

	if err := validateTokenShape(typ, policies, roles); err != nil {
		return utils.ErrorResult(err.Error())
	}

	ttl, err := parseTTL(req.GetString("expiration_ttl", ""))
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	token := &api.ACLToken{
		Name:          name,
		Type:          typ,
		Policies:      policies,
		Roles:         roleLinks(roles),
		Global:        req.GetBool("global", false),
		ExpirationTTL: ttl,
	}

	created, _, err := nomad.ACLTokens().Create(token, &api.WriteOptions{
		Region: p.ResolveRegion(ctx, req.GetString("region", "")),
	})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "create ACL token " + name,
			Kind:       "ACL token",
			Name:       name,
			Address:    p.Address(),
			Capability: "acl:write",
		}, p.Redactor()))
	}
	if created == nil || created.AccessorID == "" {
		return utils.ErrorResult(
			"Nomad accepted the request but returned no token. Run list_acl_tokens to check " +
				"whether one was created before trying again — retrying blindly is how you end " +
				"up with two.")
	}

	extra := map[string]any{"action": "created"}
	if ttl == 0 {
		extra["expiry_note"] = "This token has NO expiry and is valid until somebody revokes it. " +
			"Nomad cannot add an expiry afterwards, so if it was meant to be temporary the only " +
			"remedy is to delete it and create another with expiration_ttl set."
	}
	if typ == TypeManagement {
		extra["warning"] = "This is a MANAGEMENT token. It carries every capability in the " +
			"cluster, ignores all policies, and can mint further management tokens. Tell the user " +
			"plainly that one now exists and what its accessor ID is."
	}
	return utils.JSONResult(tokenProjection(p, created, extra))
}

// UpdateACLToken changes an existing token's name, policies or roles.
func UpdateACLToken(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("update_acl_token",
			mcp.WithDescription(
				"Change an existing ACL token's name, policies or roles, identified by its "+
					"accessor ID.\n\n"+
					"The token's secret does not change, so whoever holds it keeps working — with "+
					"different permissions from the moment this succeeds. That is what makes this "+
					"the right tool for fixing an access problem, and also what makes it "+
					"dangerous: narrowing a token's policies revokes capabilities from a "+
					"credential that is in active use, and can break running automation "+
					"immediately.\n\n"+
					"Every argument you omit is left exactly as it is. The ones you supply REPLACE "+
					"their field rather than adding to it: passing policies = [\"read-only\"] "+
					"leaves the token with that one policy and drops every other it had. Read the "+
					"token with read_acl_token first and pass the full intended list, including "+
					"the entries that should survive.\n\n"+
					"A token's type, expiry and global flag cannot be changed after creation — "+
					"those need a new token.\n\n"+
					"Confirm the resulting policy and role list with the user before calling this. "+
					"Requires the acl:write capability."),
			// Destructive: replacing the policy list discards grants Nomad
			// keeps no copy of, on a credential that is live.
			// Idempotent: applying the same change twice is the same state.
			utils.MutatingTool(true, true),
			mcp.WithString("accessor_id",
				mcp.Required(),
				mcp.Description("The accessor ID of the token to change, from list_acl_tokens."),
			),
			mcp.WithString("name",
				mcp.Description("A new human-readable name. Omit to leave the current one."),
			),
			mcp.WithArray("policies",
				mcp.WithStringItems(),
				mcp.Description(
					"The complete list of policy names the token should carry after this call. "+
						"This REPLACES the existing list. Omit the argument entirely to leave the "+
						"policies untouched."),
			),
			mcp.WithArray("roles",
				mcp.WithStringItems(),
				mcp.Description(
					"The complete list of role names the token should carry after this call. This "+
						"REPLACES the existing list. Omit the argument entirely to leave the roles "+
						"untouched."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return updateToken(ctx, req, p)
		},
	}
}

func updateToken(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	if !enabled(p) {
		return utils.ErrorResult(disabledMessage("update_acl_token"))
	}
	accessor, err := req.RequireString("accessor_id")
	if err != nil {
		return utils.ErrorResult(
			"The 'accessor_id' argument is required. Use list_acl_tokens to find it.")
	}
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}
	region := p.ResolveRegion(ctx, req.GetString("region", ""))

	// Read before write. Nomad's update endpoint takes the whole token and
	// replaces it, so sending only the changed fields would clear everything
	// else — a caller who renames a token would silently strip its policies.
	// Merging here is what makes "omitted means unchanged" true.
	current, _, err := nomad.ACLTokens().Info(accessor, &api.QueryOptions{Region: region})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "read ACL token " + utils.ShortID(accessor) + " before updating it",
			Kind:       "ACL token",
			Name:       accessor,
			Address:    p.Address(),
			Capability: "acl:read",
			ListTool:   "list_acl_tokens",
		}, p.Redactor()))
	}
	if current == nil || current.AccessorID == "" {
		return utils.ErrorResultf(
			"No ACL token with accessor ID %q exists, so there is nothing to update. Use "+
				"list_acl_tokens to see what does.", accessor)
	}

	// The SecretID is deliberately not carried into the update. Nomad keys the
	// update on the accessor ID and preserves the existing secret itself, so
	// including it would put the credential in an outgoing request body for no
	// reason — and api.ACLToken has no omitempty on that field, so "not set" is
	// the only way to keep it out.
	updated := &api.ACLToken{
		AccessorID: current.AccessorID,
		Name:       current.Name,
		Type:       current.Type,
		Policies:   current.Policies,
		Roles:      current.Roles,
		Global:     current.Global,
	}

	changed := map[string]any{}
	if name := req.GetString("name", ""); name != "" && name != current.Name {
		updated.Name = name
		changed["name"] = map[string]any{"from": current.Name, "to": name}
	}
	// Presence, not emptiness, is what distinguishes "leave it alone" from
	// "make it empty" — and an explicitly empty list is a real request, since
	// dropping every policy is how a token gets neutered without being deleted.
	if _, given := req.GetArguments()["policies"]; given {
		policies := utils.StringSlice(req, "policies")
		updated.Policies = policies
		changed["policies"] = map[string]any{"from": current.Policies, "to": policies}
	}
	if _, given := req.GetArguments()["roles"]; given {
		roles := utils.StringSlice(req, "roles")
		updated.Roles = roleLinks(roles)
		changed["roles"] = map[string]any{"from": roleLinkNames(current.Roles), "to": roles}
	}

	if len(changed) == 0 {
		return utils.ErrorResult(
			"Nothing to update: no name, policies or roles were supplied, so this call would " +
				"rewrite the token with exactly its current contents. Supply at least one of " +
				"them, or use read_acl_token if you only wanted to see what the token carries.")
	}

	if err := validateTokenShape(updated.Type, updated.Policies, roleLinkNames(updated.Roles)); err != nil {
		return utils.ErrorResult(err.Error())
	}

	result, _, err := nomad.ACLTokens().Update(updated, &api.WriteOptions{Region: region})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "update ACL token " + utils.ShortID(accessor),
			Kind:       "ACL token",
			Name:       accessor,
			Address:    p.Address(),
			Capability: "acl:write",
		}, p.Redactor()))
	}
	if result == nil || result.AccessorID == "" {
		result = updated
	}

	extra := map[string]any{
		"action":  "updated",
		"changed": changed,
		"note": "The token's secret is unchanged, so every holder of it keeps authenticating — " +
			"with these permissions from now on. Anything the token could do before and cannot " +
			"do now stopped working the moment this succeeded.",
	}
	return utils.JSONResult(tokenProjection(p, result, extra))
}

// tokenProjection renders a token for a tool response, applying the secret
// policy. extra is merged in last so a caller can add action-specific fields.
//
// Every path that returns a token goes through here. That is the point: the
// decision about the secret is made once, in one function, rather than at each
// of the three call sites where forgetting it would be a disclosure.
func tokenProjection(p *client.Provider, tok *api.ACLToken, extra map[string]any) map[string]any {
	expires, isExpired := formatExpiry(tok.ExpirationTime)

	out := map[string]any{
		"accessor_id": tok.AccessorID,
		"name":        tok.Name,
		"type":        tok.Type,
		"global":      tok.Global,
		"created":     utils.FormatTime(tok.CreateTime.UnixNano()),
	}
	if len(tok.Policies) > 0 {
		out["policies"] = tok.Policies
	}
	if names := roleLinkNames(tok.Roles); len(names) > 0 {
		out["roles"] = names
	}
	if expires != "" {
		out["expires"] = expires
		out["expired"] = isExpired
	}
	if tok.Type == TypeManagement {
		out["capabilities"] = "This is a management token: it carries every capability in the " +
			"cluster and its policies and roles, if any, are ignored."
	} else if len(tok.Policies) == 0 && len(tok.Roles) == 0 {
		out["capabilities"] = "This client token has no policies and no roles, so it grants " +
			"nothing at all. Every call made with it will be denied."
	}

	secret, note := tokenSecret(p, tok.SecretID)
	if secret != "" {
		out["secret_id"] = secret
		out["secret_warning"] = note
	} else if note != "" {
		out["secret_id_withheld"] = true
		out["secret_note"] = note
	}

	for k, v := range extra {
		out[k] = v
	}
	return out
}

// validateTokenShape rejects the combinations Nomad refuses, and the one it
// accepts but nobody means.
//
// Catching these here rather than letting Nomad answer keeps the explanation
// next to the argument that caused it: Nomad's own message for a management
// token carrying policies is terse enough that a model tends to retry it.
func validateTokenShape(typ string, policies, roles []string) error {
	switch typ {
	case TypeClient:
		if len(policies) == 0 && len(roles) == 0 {
			return errf(
				"A client token needs at least one policy or role, otherwise it grants nothing " +
					"and every call made with it is denied. Use list_acl_policies and " +
					"list_acl_roles to see what is available.")
		}
	case TypeManagement:
		if len(policies) > 0 || len(roles) > 0 {
			return errf(
				"A management token cannot carry policies or roles: it already has every " +
					"capability in the cluster and ignores them entirely. Either drop the " +
					"policies and roles, or use type=\"client\" if the intent was a scoped token — " +
					"which it usually is.")
		}
	default:
		return errf(
			"Invalid type %q: it must be %q or %q.", typ, TypeClient, TypeManagement)
	}
	return nil
}

// parseTTL reads the expiration_ttl argument. An empty value means no expiry,
// which Nomad expresses as a zero duration.
func parseTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errf(
			"Invalid expiration_ttl %q: expected a Go duration such as \"8h\", \"720h\" or "+
				"\"30m\". Omit it entirely for a token that never expires.", raw)
	}
	if d <= 0 {
		return 0, errf(
			"Invalid expiration_ttl %q: it must be greater than zero. Omit the argument for a "+
				"token that never expires.", raw)
	}
	return d, nil
}
