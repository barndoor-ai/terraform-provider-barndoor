// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmLifetimeOverrideKind is what differs between the group and user
// override resources: the subject they override for, and its API path.
type llmLifetimeOverrideKind struct {
	typeSuffix  string // resource type name after the provider prefix
	segment     string // API path segment: "groups" or "users"
	subjectAttr string // schema attribute and wire field: "group_name" or "user_id"
	noun        string // "group" or "user", for messages
	subjectDoc  string
	description string
}

var (
	llmLifetimeGroupOverride = llmLifetimeOverrideKind{
		typeSuffix:  "_llm_api_key_lifetime_group_override",
		segment:     "groups",
		subjectAttr: "group_name",
		noun:        "group",
		subjectDoc: "IdP group whose members this override applies to. The platform allows one override per " +
			"group and answers 409 on a second. Changing it forces a new override.",
		description: "Manages an LLM Gateway API key lifetime **group override**: the policy for members " +
			"of one IdP group, replacing the organization default for them.",
	}
	llmLifetimeUserOverride = llmLifetimeOverrideKind{
		typeSuffix:  "_llm_api_key_lifetime_user_override",
		segment:     "users",
		subjectAttr: "user_id",
		noun:        "user",
		subjectDoc: "ID of the user this override applies to; they must belong to the organization. The " +
			"platform allows one override per user and answers 409 on a second. Changing it forces a new " +
			"override.",
		description: "Manages an LLM Gateway API key lifetime **user override**: the policy for one user. " +
			"It wins over that user's group overrides and the organization default, so it can grant an " +
			"exception in either direction.",
	}
)

// llmUserIDFormat checks a user_id is a UUID before the API is asked.
var llmUserIDFormat = stringvalidator.RegexMatches(
	regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`),
	"must be a user ID (a UUID)",
)

var (
	_ resource.Resource                     = &llmAPIKeyLifetimeOverrideResource{}
	_ resource.ResourceWithConfigure        = &llmAPIKeyLifetimeOverrideResource{}
	_ resource.ResourceWithImportState      = &llmAPIKeyLifetimeOverrideResource{}
	_ resource.ResourceWithConfigValidators = &llmAPIKeyLifetimeOverrideResource{}
)

// NewLlmAPIKeyLifetimeGroupOverrideResource returns a new
// barndoor_llm_api_key_lifetime_group_override resource.
func NewLlmAPIKeyLifetimeGroupOverrideResource() resource.Resource {
	return &llmAPIKeyLifetimeOverrideResource{kind: llmLifetimeGroupOverride}
}

// NewLlmAPIKeyLifetimeUserOverrideResource returns a new
// barndoor_llm_api_key_lifetime_user_override resource.
func NewLlmAPIKeyLifetimeUserOverrideResource() resource.Resource {
	return &llmAPIKeyLifetimeOverrideResource{kind: llmLifetimeUserOverride}
}

type llmAPIKeyLifetimeOverrideResource struct {
	client *client.Client
	kind   llmLifetimeOverrideKind
}

func (r *llmAPIKeyLifetimeOverrideResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + r.kind.typeSuffix
}

func (r *llmAPIKeyLifetimeOverrideResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attributes := llmLifetimeLimitAttributes()
	attributes["id"] = schema.StringAttribute{
		MarkdownDescription: "Override UUID assigned by the API; also the `terraform import` key.",
		Computed:            true,
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.UseStateForUnknown(),
		},
	}
	subject := schema.StringAttribute{
		MarkdownDescription: r.kind.subjectDoc,
		Required:            true,
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.RequiresReplace(),
		},
		Validators: []validator.String{
			stringvalidator.LengthAtLeast(1),
		},
	}
	if r.kind.subjectAttr == "user_id" {
		subject.Validators = append(subject.Validators, llmUserIDFormat)
	}
	attributes[r.kind.subjectAttr] = subject

	resp.Schema = schema.Schema{
		MarkdownDescription: r.kind.description + "\n\n" + llmLifetimePolicyIntro,
		Attributes:          attributes,
	}
}

func (r *llmAPIKeyLifetimeOverrideResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{llmLifetimeFixedNeedsMaxValidator{}}
}

func (r *llmAPIKeyLifetimeOverrideResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if c := configureLlmClient(req, resp); c != nil {
		r.client = c
	}
}

func (r *llmAPIKeyLifetimeOverrideResource) what() string {
	return "API key lifetime " + r.kind.noun + " override"
}

func (r *llmAPIKeyLifetimeOverrideResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireLlmClient(r.client, &resp.Diagnostics) {
		return
	}
	limit := limitWireFrom(ctx, req.Plan, &resp.Diagnostics)
	var subject types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root(r.kind.subjectAttr), &subject)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := map[string]any{
		r.kind.subjectAttr:  strings.TrimSpace(subject.ValueString()),
		"max_lifetime_days": limit.MaxLifetimeDays,
		"require_expiry":    limit.RequireExpiry,
		"fixed_lifetime":    limit.FixedLifetime,
	}
	var row llmLifetimeOverrideRow
	if err := doJSON(ctx, r.client, http.MethodPost, llmAPIKeyLifetimePath+"/"+r.kind.segment, body, &row); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, r.what(), "create the "+r.what(), err)
		return
	}
	if row.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no override id.")
		return
	}
	r.setState(ctx, &resp.State, &row, subject, &resp.Diagnostics)
}

// Read finds the override in the org-wide listing: the API has no get-by-id.
func (r *llmAPIKeyLifetimeOverrideResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireLlmClient(r.client, &resp.Diagnostics) {
		return
	}
	var id, subject types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root(r.kind.subjectAttr), &subject)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policies llmLifetimePolicies
	if err := doJSON(ctx, r.client, http.MethodGet, llmAPIKeyLifetimePath, nil, &policies); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, r.what(), "list API key lifetime policies", err)
		return
	}
	for _, row := range r.rows(&policies) {
		if row.ID == id.ValueString() {
			r.setState(ctx, &resp.State, &row, subject, &resp.Diagnostics)
			return
		}
	}
	// Deleted out-of-band; drop the resource so Terraform plans a recreate.
	resp.State.RemoveResource(ctx)
}

func (r *llmAPIKeyLifetimeOverrideResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !requireLlmClient(r.client, &resp.Diagnostics) {
		return
	}
	limit := limitWireFrom(ctx, req.Plan, &resp.Diagnostics)
	var id, subject types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("id"), &id)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root(r.kind.subjectAttr), &subject)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var row llmLifetimeOverrideRow
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmAPIKeyLifetimePath+"/"+r.kind.segment+"/"+id.ValueString(), &limit, &row); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, r.what(), "update the "+r.what(), err)
		return
	}
	r.setState(ctx, &resp.State, &row, subject, &resp.Diagnostics)
}

// Delete removes the override. A 404 means it is already gone, which is
// success for a destroy.
func (r *llmAPIKeyLifetimeOverrideResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !requireLlmClient(r.client, &resp.Diagnostics) {
		return
	}
	var id types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := doJSON(ctx, r.client, http.MethodDelete,
		llmAPIKeyLifetimePath+"/"+r.kind.segment+"/"+id.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, r.what(), "delete the "+r.what(), err)
	}
}

func (r *llmAPIKeyLifetimeOverrideResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// llmLifetimeOverrideRow is a group or user override as this resource sees
// it: whichever subject field applies is set.
type llmLifetimeOverrideRow struct {
	ID        string  `json:"id"`
	GroupName *string `json:"group_name"`
	UserID    *string `json:"user_id"`
	llmLifetimeLimitWire
}

func (r *llmAPIKeyLifetimeOverrideResource) rows(p *llmLifetimePolicies) []llmLifetimeOverrideRow {
	var out []llmLifetimeOverrideRow
	if r.kind.segment == "groups" {
		for _, g := range p.Groups {
			out = append(out, llmLifetimeOverrideRow{ID: g.ID, GroupName: g.GroupName, llmLifetimeLimitWire: g.llmLifetimeLimitWire})
		}
		return out
	}
	for _, u := range p.Users {
		userID := u.UserID
		out = append(out, llmLifetimeOverrideRow{ID: u.ID, UserID: &userID, llmLifetimeLimitWire: u.llmLifetimeLimitWire})
	}
	return out
}

// setState records the server's view. The subject keeps the configured
// spelling while it still names the same group or user, so a difference the
// platform normalizes away doesn't plan a replacement: it trims a group name,
// and a UUID may come back in a different case. Group names are otherwise
// case-sensitive.
func (r *llmAPIKeyLifetimeOverrideResource) setState(ctx context.Context, state *tfsdk.State, row *llmLifetimeOverrideRow, prior types.String, diags *diag.Diagnostics) {
	server := row.GroupName
	if r.kind.subjectAttr == "user_id" {
		server = row.UserID
	}
	subject := prior
	if server != nil {
		p, known := knownString(prior)
		p = strings.TrimSpace(p)
		same := known && (p == *server || (r.kind.subjectAttr == "user_id" && strings.EqualFold(p, *server)))
		if !same {
			subject = types.StringValue(*server)
		}
	}
	diags.Append(state.SetAttribute(ctx, path.Root("id"), types.StringValue(row.ID))...)
	diags.Append(state.SetAttribute(ctx, path.Root(r.kind.subjectAttr), subject)...)
	setLimitState(ctx, state, row.llmLifetimeLimitWire, diags)
}
