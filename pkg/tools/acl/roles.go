// Copyright (c) 2026 suyash1603
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"context"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/suyash1603/nomad-mcp-server/pkg/client"
	"github.com/suyash1603/nomad-mcp-server/pkg/utils"
)

// roleSummary is the list projection. A role is small enough that the list and
// the read return nearly the same thing, which is why there is no truncation
// here and no separate "summary" concept beyond the shape.
type roleSummary struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Policies    []string `json:"policies,omitempty"`
}

// ListACLRoles lists the roles defined in the cluster.
func ListACLRoles(p *client.Provider) server.ServerTool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(
			"List the ACL roles defined in this cluster, with the policies each one bundles.\n\n" +
				"A role is a named group of ACL policies. Tokens link to roles instead of listing " +
				"policies individually, which means changing a role's policy list changes what " +
				"every token carrying it can do — without touching a single token.\n\n" +
				"Use this when working out how access is organised before changing anything: if a " +
				"role already grants what someone needs, attaching it to their token is a smaller " +
				"and more reversible change than writing a new policy.\n\n" +
				"Requires the acl:read capability."),
		utils.ReadOnlyTool(),
		utils.RegionParam(),
	}
	opts = append(opts, utils.PageParams()...)

	return server.ServerTool{
		Tool: mcp.NewTool("list_acl_roles", opts...),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !enabled(p) {
				return utils.ErrorResult(disabledMessage("list_acl_roles"))
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			region := p.ResolveRegion(ctx, req.GetString("region", ""))
			q := utils.PageFrom(req).Apply(&api.QueryOptions{Region: region})

			roles, meta, err := nomad.ACLRoles().List(q)
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "list ACL roles",
					Kind:       "ACL role",
					Address:    p.Address(),
					Capability: "acl:read",
				}, p.Redactor()))
			}

			items := make([]roleSummary, 0, len(roles))
			for _, r := range roles {
				if r == nil {
					continue
				}
				items = append(items, roleSummary{
					ID:          r.ID,
					Name:        r.Name,
					Description: r.Description,
					Policies:    policyLinkNames(r.Policies),
				})
			}

			result := utils.List{Count: len(items), Region: region, Items: items}
			if meta != nil {
				result.NextToken = meta.NextToken
				result.Note = utils.NextTokenNote(meta.NextToken, len(items))
			}
			if result.Note == "" && len(items) == 0 {
				result.Note = "No ACL roles are defined. Tokens in this cluster therefore " +
					"reference policies directly, which is normal on smaller clusters."
			}
			return utils.JSONResult(result)
		},
	}
}

// ReadACLRole reads one role, by name or by ID.
func ReadACLRole(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("read_acl_role",
			mcp.WithDescription(
				"Read one ACL role: the policies it bundles, its description, and the ID needed "+
					"to update it.\n\n"+
					"Identify the role by name, which is what people use, or by ID. Nomad "+
					"generates the ID at creation and it is the only way to update a role, so "+
					"this is the call that supplies it.\n\n"+
					"Follow up with read_acl_policy on the policy names returned here to see the "+
					"actual grants — a role is only a bundle, and contains no capabilities of its "+
					"own.\n\n"+
					"Requires the acl:read capability."),
			utils.ReadOnlyTool(),
			mcp.WithString("name",
				mcp.Description(
					"The role's name, as shown by list_acl_roles. Give either this or 'id'."),
			),
			mcp.WithString("id",
				mcp.Description(
					"The role's Nomad-generated ID. Give either this or 'name'; the ID wins if "+
						"both are supplied."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !enabled(p) {
				return utils.ErrorResult(disabledMessage("read_acl_role"))
			}
			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			role, err := lookupRole(ctx, req, p, nomad)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			policies := policyLinkNames(role.Policies)
			out := map[string]any{
				"id":                           role.ID,
				"name":                         role.Name,
				"description":                  role.Description,
				"policies":                     policies,
				"policies_are_untrusted_input": untrustedInputNote,
			}
			if len(policies) == 0 {
				out["note"] = "This role bundles no policies, so a token carrying it gains " +
					"nothing from it."
			} else {
				out["note"] = "A role grants nothing by itself. These policies apply to every " +
					"token linked to this role; read_acl_policy shows what each one actually " +
					"allows."
			}
			return utils.JSONResult(out)
		},
	}
}

// CreateACLRole creates a new role bundling existing policies.
func CreateACLRole(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("create_acl_role",
			mcp.WithDescription(
				"Create an ACL role: a named bundle of existing ACL policies that tokens can link "+
					"to.\n\n"+
					"Creating a role is additive and grants nobody anything on its own — a role "+
					"takes effect only once a token links to it, which is a separate call to "+
					"create_acl_token or update_acl_token. That makes this the safest of the ACL "+
					"write tools and a good way to prepare a change before anyone is affected by "+
					"it.\n\n"+
					"Every policy named must already exist; create them with write_acl_policy "+
					"first. Role names are unique across the whole federated cluster, so creating "+
					"one whose name is taken fails rather than replacing it — use update_acl_role "+
					"to change an existing role.\n\n"+
					"Requires the acl:write capability."),
			// Not destructive: it adds a bundle nothing references yet.
			// Not idempotent: Nomad rejects a second create at the same name,
			// so a blind retry fails rather than converging.
			utils.MutatingTool(false, false),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description(
					"The role's name, unique across the cluster. This is what people refer to it "+
						"by when linking tokens."),
			),
			mcp.WithArray("policies",
				mcp.Required(),
				mcp.WithStringItems(),
				mcp.Description(
					"Names of the ACL policies this role bundles, from list_acl_policies. At "+
						"least one is required, and each must already exist."),
			),
			mcp.WithString("description",
				mcp.Description("A human-readable description of what this role is for."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !enabled(p) {
				return utils.ErrorResult(disabledMessage("create_acl_role"))
			}
			name, err := req.RequireString("name")
			if err != nil {
				return utils.ErrorResult("The 'name' argument is required.")
			}
			if strings.TrimSpace(name) == "" {
				return utils.ErrorResult("The 'name' argument cannot be empty.")
			}
			policies := utils.StringSlice(req, "policies")
			if len(policies) == 0 {
				return utils.ErrorResult(
					"The 'policies' argument is required and must name at least one existing ACL " +
						"policy: a role with no policies bundles nothing and grants nothing. Use " +
						"list_acl_policies to see what is available.")
			}

			nomad, err := p.FromContext(ctx)
			if err != nil {
				return utils.ErrorResult(err.Error())
			}

			created, _, err := nomad.ACLRoles().Create(&api.ACLRole{
				Name:        name,
				Description: req.GetString("description", ""),
				Policies:    policyLinks(policies),
			}, &api.WriteOptions{Region: p.ResolveRegion(ctx, req.GetString("region", ""))})
			if err != nil {
				return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
					Op:         "create ACL role " + name,
					Kind:       "ACL role",
					Name:       name,
					Address:    p.Address(),
					Capability: "acl:write",
				}, p.Redactor()))
			}
			if created == nil || created.ID == "" {
				return utils.ErrorResult(
					"Nomad accepted the request but returned no role. Run list_acl_roles to check " +
						"whether it was created before retrying.")
			}

			return utils.JSONResult(map[string]any{
				"id":       created.ID,
				"name":     created.Name,
				"policies": policyLinkNames(created.Policies),
				"action":   "created",
				"note": "The role exists but affects nobody yet: link it to a token with " +
					"create_acl_token or update_acl_token for it to grant anything. Keep the ID " +
					"above — updating a role requires it.",
			})
		},
	}
}

// UpdateACLRole changes an existing role's policy list, name or description.
func UpdateACLRole(p *client.Provider) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("update_acl_role",
			mcp.WithDescription(
				"Change an existing ACL role's policies, name or description.\n\n"+
					"This is the widest-reaching ACL write there is. A role's policy list applies "+
					"to every token linked to it, so removing a policy here revokes it from all of "+
					"them at once, with no rollout and nothing to roll back to — Nomad keeps no "+
					"previous version. Confirm the resulting policy list with the user, and check "+
					"who is affected with list_acl_tokens first.\n\n"+
					"The policies you supply REPLACE the role's existing list rather than adding "+
					"to it. Read the role with read_acl_role and pass the full intended list, "+
					"including the entries that should survive. Arguments you omit are left "+
					"unchanged.\n\n"+
					"Identify the role by name or by ID; Nomad requires the ID internally and this "+
					"tool looks it up from the name when you give one.\n\n"+
					"Requires the acl:write capability."),
			// Destructive: the discarded policy list is state Nomad does not
			// keep, and the revocation reaches every linked token at once.
			// Idempotent: writing the same list twice is the same state.
			utils.MutatingTool(true, true),
			mcp.WithString("name",
				mcp.Description(
					"The role to change, by name. Give either this or 'id'. To RENAME a role, "+
						"identify it by 'id' and pass the new name in 'new_name'."),
			),
			mcp.WithString("id",
				mcp.Description(
					"The role to change, by its Nomad-generated ID, from read_acl_role or "+
						"list_acl_roles. The ID wins if both are supplied."),
			),
			mcp.WithString("new_name",
				mcp.Description(
					"A new name for the role. Omit to leave it as it is. Renaming does not affect "+
						"tokens already linked, which reference the role by ID."),
			),
			mcp.WithArray("policies",
				mcp.WithStringItems(),
				mcp.Description(
					"The complete list of policy names the role should bundle after this call. "+
						"This REPLACES the existing list. Omit the argument entirely to leave the "+
						"policies untouched."),
			),
			mcp.WithString("description",
				mcp.Description("A new description. Omit to leave the current one."),
			),
			utils.RegionParam(),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return updateRole(ctx, req, p)
		},
	}
}

func updateRole(ctx context.Context, req mcp.CallToolRequest, p *client.Provider) (*mcp.CallToolResult, error) {
	if !enabled(p) {
		return utils.ErrorResult(disabledMessage("update_acl_role"))
	}
	nomad, err := p.FromContext(ctx)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	// Read before write, for the same reason as update_acl_token: Nomad's
	// update replaces the whole role, so merging here is what makes an omitted
	// argument mean "unchanged" rather than "cleared". It also supplies the ID,
	// which the update endpoint requires and which callers identifying the role
	// by name do not have.
	current, err := lookupRole(ctx, req, p, nomad)
	if err != nil {
		return utils.ErrorResult(err.Error())
	}

	updated := &api.ACLRole{
		ID:          current.ID,
		Name:        current.Name,
		Description: current.Description,
		Policies:    current.Policies,
	}

	changed := map[string]any{}
	if newName := strings.TrimSpace(req.GetString("new_name", "")); newName != "" && newName != current.Name {
		updated.Name = newName
		changed["name"] = map[string]any{"from": current.Name, "to": newName}
	}
	if _, given := req.GetArguments()["description"]; given {
		desc := req.GetString("description", "")
		if desc != current.Description {
			updated.Description = desc
			changed["description"] = map[string]any{"from": current.Description, "to": desc}
		}
	}
	if _, given := req.GetArguments()["policies"]; given {
		policies := utils.StringSlice(req, "policies")
		if len(policies) == 0 {
			return utils.ErrorResult(
				"A role must bundle at least one policy. Passing an empty list would strip every " +
					"grant from each token linked to this role while leaving the link in place, " +
					"which is almost never what anyone means — if the intent is to revoke access, " +
					"change the tokens with update_acl_token, or remove the role from them.")
		}
		updated.Policies = policyLinks(policies)
		changed["policies"] = map[string]any{
			"from": policyLinkNames(current.Policies),
			"to":   policies,
		}
	}

	if len(changed) == 0 {
		return utils.ErrorResult(
			"Nothing to update: no new_name, policies or description were supplied, so this call " +
				"would rewrite the role with exactly its current contents. Supply at least one of " +
				"them, or use read_acl_role if you only wanted to see what the role bundles.")
	}

	result, _, err := nomad.ACLRoles().Update(updated, &api.WriteOptions{
		Region: p.ResolveRegion(ctx, req.GetString("region", "")),
	})
	if err != nil {
		return utils.ErrorResult(utils.MapError(err, utils.ErrorContext{
			Op:         "update ACL role " + current.Name,
			Kind:       "ACL role",
			Name:       current.Name,
			Address:    p.Address(),
			Capability: "acl:write",
		}, p.Redactor()))
	}
	if result == nil || result.ID == "" {
		result = updated
	}

	out := map[string]any{
		"id":       result.ID,
		"name":     result.Name,
		"policies": policyLinkNames(result.Policies),
		"action":   "updated",
		"changed":  changed,
	}
	if _, policiesChanged := changed["policies"]; policiesChanged {
		out["warning"] = "Every token linked to this role now carries exactly the policies " +
			"listed above. Any policy the role bundled before and does not bundle now has been " +
			"revoked from all of those tokens, as of now. Nomad keeps no previous version of a " +
			"role, so undoing this means writing the old list back by hand."
	}
	return utils.JSONResult(out)
}

// lookupRole resolves the role a call refers to, by ID or by name.
//
// Both write and read paths need this, and both need the same error when
// neither argument is given — a role identified by nothing is the easiest
// mistake to make here, because Nomad's own CLI accepts a bare name.
func lookupRole(ctx context.Context, req mcp.CallToolRequest, p *client.Provider, nomad *api.Client) (*api.ACLRole, error) {
	id := strings.TrimSpace(req.GetString("id", ""))
	name := strings.TrimSpace(req.GetString("name", ""))

	if id == "" && name == "" {
		return nil, errf(
			"Identify the role with either 'name' or 'id'. Use list_acl_roles to see what exists.")
	}

	q := &api.QueryOptions{Region: p.ResolveRegion(ctx, req.GetString("region", ""))}

	var (
		role *api.ACLRole
		err  error
	)
	if id != "" {
		role, _, err = nomad.ACLRoles().Get(id, q)
	} else {
		role, _, err = nomad.ACLRoles().GetByName(name, q)
	}

	ref := name
	if id != "" {
		ref = id
	}
	if err != nil {
		return nil, errf("%s", utils.MapError(err, utils.ErrorContext{
			Op:         "read ACL role " + ref,
			Kind:       "ACL role",
			Name:       ref,
			Address:    p.Address(),
			Capability: "acl:read",
			ListTool:   "list_acl_roles",
		}, p.Redactor()))
	}
	if role == nil || role.ID == "" {
		return nil, errf(
			"No ACL role %q exists. Use list_acl_roles to see what does.", ref)
	}
	return role, nil
}

// policyLinks builds the links for a write from a list of policy names.
func policyLinks(names []string) []*api.ACLRolePolicyLink {
	out := make([]*api.ACLRolePolicyLink, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, &api.ACLRolePolicyLink{Name: n})
		}
	}
	return out
}
