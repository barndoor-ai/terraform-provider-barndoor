// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// llmChangeNoteMaxChars is the platform's normalize_change_note cap.
const llmChangeNoteMaxChars = 500

// llmChangeNoteAttribute builds the change_note attribute shared by the LLM
// Gateway configuration resources (BCP-3998). The platform stamps the note
// onto the row as last_change_note and into the audit event, and an update
// that omits it clears the previous note — so it annotates one change rather
// than describing the object. It is therefore write-only here: sent on every
// create and update, never refreshed, so a note left by an edit in the app
// does not show up as drift.
func llmChangeNoteAttribute(what string) schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "Optional note explaining the change (\"Raise cap for Q4 launch\"), recorded " +
			"on the " + what + " and in the audit trail (at most 500 characters). It describes the write " +
			"Terraform makes, not the " + what + ": the platform clears the previous note on any update " +
			"that does not send one, and the value is not refreshed from the platform. Changing only the " +
			"note makes an in-place update that records it.",
		Optional: true,
		Validators: []validator.String{
			stringvalidator.UTF8LengthAtMost(llmChangeNoteMaxChars),
		},
	}
}

// llmTargetAttributes builds the target dimensions shared by the token-budget
// and rate-limit resources (BCP-2813, BCP-4053): narrowing a rule from every
// request in its scope to the requests routed to one provider, upstream
// model, caller alias, or MCP server. what names the resource kind. The
// budget update API does not accept them, so createOnly makes every change a
// replacement; the rate-limit update API clears them with explicit nulls.
func llmTargetAttributes(what string, createOnly bool) map[string]schema.Attribute {
	suffix := " Can be changed or removed in place."
	var modifiers []planmodifier.String
	if createOnly {
		suffix = " Changing or removing it forces a new " + what + " (the API cannot update it)."
		modifiers = []planmodifier.String{stringplanmodifier.RequiresReplace()}
	}
	attr := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{
			MarkdownDescription: desc + suffix,
			Optional:            true,
			Validators:          []validator.String{noSurroundingWhitespace},
			PlanModifiers:       modifiers,
		}
	}
	return map[string]schema.Attribute{
		"target_provider_id": attr("Narrows the " + what + " to requests routed to this " +
			"`barndoor_llm_provider`. Combine it with `target_upstream_model` to narrow further to one " +
			"of that provider's models."),
		"target_upstream_model": attr("Narrows the " + what + " to requests whose route resolves to this " +
			"upstream model name. Requires `target_provider_id`: without it, every provider serving a " +
			"model of that name would share one counter."),
		"target_model_alias": attr("Narrows the " + what + " to requests that name this model alias " +
			"(the `model` the caller sent, before routing — a routing policy's alias counts, not the " +
			"model it routes to). Cannot be combined with `target_provider_id` or " +
			"`target_upstream_model`."),
		"target_mcp_server_id": attr("Narrows the " + what + " to MCP tool traffic sent to this " +
			"`barndoor_mcp_server`. Cannot be combined with the model targets, and requires " +
			"`traffic_type` `mcp` or `all`."),
	}
}

// llmTargetShapeValidator mirrors the platform's validate_target_shape (and
// the V54/V73 CHECKs behind it). Checking at plan time matters beyond the
// earlier error: the budget update handler runs no shape check, so a
// traffic_type change that contradicts a stored target reaches the CHECK and
// surfaces as an opaque 500.
type llmTargetShapeValidator struct{}

func (v llmTargetShapeValidator) Description(_ context.Context) string {
	return "target_* attributes must form a shape the platform accepts"
}

func (v llmTargetShapeValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v llmTargetShapeValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var providerID, upstream, alias, mcpServer, trafficType types.String
	for name, dst := range map[string]*types.String{
		"target_provider_id":    &providerID,
		"target_upstream_model": &upstream,
		"target_model_alias":    &alias,
		"target_mcp_server_id":  &mcpServer,
		"traffic_type":          &trafficType,
	} {
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(name), dst)...)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// set reports a configured target; an unknown value (a reference to a
	// resource not yet created) is set too, since only its value is unknown.
	set := func(s types.String) bool { return !s.IsNull() }
	llmTarget := set(providerID) || set(upstream) || set(alias)
	tt, ttKnown := knownString(trafficType)

	switch {
	case set(alias) && (set(providerID) || set(upstream)):
		resp.Diagnostics.AddAttributeError(path.Root("target_model_alias"),
			"target_model_alias cannot be combined with a provider target",
			"A rule targets either the alias the caller sent or the route it resolved to, not both. "+
				"Remove target_model_alias, or remove target_provider_id and target_upstream_model.")
	case set(upstream) && !set(providerID):
		resp.Diagnostics.AddAttributeError(path.Root("target_upstream_model"),
			"target_upstream_model requires target_provider_id",
			"An upstream model name alone would pool every provider that serves a model of that name "+
				"into one counter. Set target_provider_id to the provider whose model this is.")
	}
	if set(mcpServer) && llmTarget {
		resp.Diagnostics.AddAttributeError(path.Root("target_mcp_server_id"),
			"target_mcp_server_id cannot be combined with a model target",
			"MCP tool traffic has no model route, so a rule narrows to an MCP server or to a model "+
				"target, not both.")
	}
	if !ttKnown {
		return
	}
	if llmTarget && tt == "mcp" {
		resp.Diagnostics.AddAttributeError(path.Root("traffic_type"),
			"Model targets need model traffic",
			"traffic_type = \"mcp\" matches only MCP tool traffic, which never reaches a model route, so "+
				"a model target would never match. Use traffic_type \"llm\" or \"all\".")
	}
	if set(mcpServer) && tt == "llm" {
		resp.Diagnostics.AddAttributeError(path.Root("traffic_type"),
			"target_mcp_server_id needs MCP traffic",
			"traffic_type = \"llm\" matches only model traffic, which never reaches an MCP server, so the "+
				"target would never match. Use traffic_type \"mcp\" or \"all\".")
	}
}
