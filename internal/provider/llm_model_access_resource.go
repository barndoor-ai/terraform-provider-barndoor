// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/defaults"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmModelAccessTargetKinds are the target discriminators of a model-access
// policy (the ModelAccessTarget enum's `kind` tags).
var llmModelAccessTargetKinds = []string{"model_alias", "model", "provider", "provider_model", "route_group"}

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &llmModelAccessResource{}
	_ resource.ResourceWithConfigure      = &llmModelAccessResource{}
	_ resource.ResourceWithImportState    = &llmModelAccessResource{}
	_ resource.ResourceWithValidateConfig = &llmModelAccessResource{}
)

// NewLlmModelAccessResource returns a new barndoor_llm_model_access resource.
func NewLlmModelAccessResource() resource.Resource {
	return &llmModelAccessResource{}
}

// llmModelAccessResource manages an LLM Gateway model-access policy (an
// allowlist or denylist of models/providers for an identity scope) through
// the llm-gateway-service admin REST API (`/api/llm-gateway/admin/model-access`).
type llmModelAccessResource struct {
	client *client.Client
}

// llmModelAccessResourceModel maps the resource schema to Go types.
type llmModelAccessResourceModel struct {
	ID          types.String                `tfsdk:"id"`
	OrgID       types.String                `tfsdk:"org_id"`
	Name        types.String                `tfsdk:"name"`
	ScopeType   types.String                `tfsdk:"scope_type"`
	ScopeID     types.String                `tfsdk:"scope_id"`
	ScopeValue  types.String                `tfsdk:"scope_value"`
	PolicyType  types.String                `tfsdk:"policy_type"`
	Targets     []llmModelAccessTargetModel `tfsdk:"targets"`
	TrafficType types.String                `tfsdk:"traffic_type"`
	Enabled     types.Bool                  `tfsdk:"enabled"`
	ChangeNote  types.String                `tfsdk:"change_note"`
}

// llmModelAccessTargetModel maps one entry of the targets list.
type llmModelAccessTargetModel struct {
	Kind       types.String `tfsdk:"kind"`
	Alias      types.String `tfsdk:"alias"`
	Model      types.String `tfsdk:"model"`
	ProviderID types.String `tfsdk:"provider_id"`
	GroupID    types.String `tfsdk:"group_id"`
}

func (r *llmModelAccessResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_model_access"
}

func (r *llmModelAccessResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway model-access policy: an allowlist or denylist of " +
			"models and providers, applied to an identity scope (the organization, a user, an IdP group, " +
			"…).\n\n" +
			"`scope_type`/`scope_id`/`scope_value` answer *who* the policy applies to; the `targets` " +
			"list answers *what* is allowed or denied and is OR-ed when matching. A `route_group` target " +
			"stands for the current members of a `barndoor_llm_model_route_group`, so editing the group " +
			"changes what the policy covers without an edit to the policy. A cleared `scope_id` " +
			"or `scope_value` cannot be unset through the update API, so removing one forces a new policy.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Policy UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the policy belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Human-readable policy name.",
				Required:            true,
			},
			"scope_type": llmScopeTypeAttribute(llmGatewayIdentityScopeTypes, false),
			"scope_id":   llmScopeIDAttribute(),
			"scope_value": llmScopeValueAttribute("e.g. a role or IdP group name for `role`/`group` " +
				"scopes"),
			"policy_type": schema.StringAttribute{
				MarkdownDescription: "`allowlist` (only the targets are permitted) or `denylist` (the " +
					"targets are blocked).",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf("allowlist", "denylist"),
				},
			},
			"targets": schema.ListNestedAttribute{
				MarkdownDescription: "What the policy allows or denies; entries are OR-ed when matching. " +
					"At least one is required.",
				Required:     true,
				NestedObject: llmModelAccessTargetNestedObject(),
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
				},
			},
			"traffic_type": llmTrafficTypeAttribute("llm", stringdefault.StaticString("llm")),
			"enabled":      llmEnabledAttribute(),
			"change_note":  llmChangeNoteAttribute("policy"),
		},
	}
}

// llmModelAccessTargetNestedObject defines the schema for one targets entry.
func llmModelAccessTargetNestedObject() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"kind": schema.StringAttribute{
				MarkdownDescription: "Target dimension: `model_alias` (matches the caller-facing alias; " +
					"requires `alias`), `model` (matches the resolved upstream model; requires `model`), " +
					"`provider` (any model on the provider; requires `provider_id`), `provider_model` " +
					"(an alias on a specific provider; requires `provider_id` and `model`), or `route_group` " +
					"(every route alias in a `barndoor_llm_model_route_group`; requires `group_id`).",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf(llmModelAccessTargetKinds...),
				},
			},
			"alias": schema.StringAttribute{
				MarkdownDescription: "Caller-facing model alias to match, with an optional trailing `*` " +
					"wildcard (`model_alias` targets).",
				Optional: true,
			},
			"model": schema.StringAttribute{
				MarkdownDescription: "Model name to match, with an optional trailing `*` wildcard: the " +
					"resolved upstream model for `model` targets, the caller-facing alias for " +
					"`provider_model` targets.",
				Optional: true,
			},
			"provider_id": schema.StringAttribute{
				MarkdownDescription: "UUID of the `barndoor_llm_provider` to match (`provider` and " +
					"`provider_model` targets).",
				Optional: true,
			},
			"group_id": schema.StringAttribute{
				MarkdownDescription: "UUID of the `barndoor_llm_model_route_group` whose members to match " +
					"(`route_group` targets). The target expands to one `model_alias` target per member " +
					"when the policy is evaluated. The platform does not check that the group exists: an " +
					"empty group, or one that has been deleted, expands to no targets. An `allowlist` whose " +
					"only target is such a group therefore denies every model to its scope.",
				Optional: true,
			},
		},
	}
}

// ValidateConfig checks each target's shape at plan time, so a kind missing
// its field (or carrying another kind's) fails before any API call. Fields
// that are still unknown (for example a group_id from a route group created
// in the same apply) count as set.
func (r *llmModelAccessResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var targets types.List
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("targets"), &targets)...)
	if resp.Diagnostics.HasError() || targets.IsNull() || targets.IsUnknown() {
		return
	}

	for i, elem := range targets.Elements() {
		obj, ok := elem.(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() {
			continue
		}
		var t llmModelAccessTargetModel
		resp.Diagnostics.Append(obj.As(ctx, &t, basetypes.ObjectAsOptions{})...)
		if resp.Diagnostics.HasError() {
			return
		}
		if t.Kind.IsNull() || t.Kind.IsUnknown() {
			continue
		}
		checkLlmModelAccessTargetShape(i, t, func(v types.String) bool { return !v.IsNull() }, &resp.Diagnostics)
	}
}

func (r *llmModelAccessResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	r.client = c
}

func (r *llmModelAccessResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmModelAccessResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	targets := buildLlmModelAccessTargets(plan.Targets, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	body := &llmModelAccessCreateRequest{
		Name:       plan.Name.ValueString(),
		ScopeType:  plan.ScopeType.ValueString(),
		PolicyType: plan.PolicyType.ValueString(),
		Targets:    targets,
		ChangeNote: stringPtrIfKnown(plan.ChangeNote),
	}
	if v, ok := knownString(plan.ScopeID); ok {
		body.ScopeID = &v
	}
	if v, ok := knownString(plan.ScopeValue); ok {
		body.ScopeValue = &v
	}
	if v, ok := knownString(plan.TrafficType); ok {
		body.TrafficType = &v
	}

	var policy llmModelAccessResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/model-access", body, &policy); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model-access policy", "create the LLM model-access policy", err)
		return
	}
	if policy.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no policy id.")
		return
	}

	// Record the created policy before any follow-up call so a failure below
	// still leaves it tracked rather than orphaned.
	state := applyLlmModelAccessResponse(&policy, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The create endpoint has no `enabled` field (new policies are always
	// enabled); converge an `enabled = false` plan with an immediate update.
	if enabled, ok := knownBool(plan.Enabled); ok && !enabled {
		disable := false
		if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/model-access/"+policy.ID,
			&llmModelAccessUpdateRequest{Enabled: &disable}, &policy); err != nil {
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM model-access policy",
				"disable the LLM model-access policy after create", err)
			return
		}
		state = applyLlmModelAccessResponse(&policy, &plan)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	}
}

// Read refreshes the policy from the org-wide listing: the admin API has no
// get-by-id endpoint for model-access policies.
func (r *llmModelAccessResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmModelAccessResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policies []llmModelAccessResponse
	if err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/model-access", nil, &policies); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model-access policy", "list LLM model-access policies", err)
		return
	}

	for i := range policies {
		if policies[i].ID == state.ID.ValueString() {
			newState := applyLlmModelAccessResponse(&policies[i], &state)
			resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
			return
		}
	}

	// Deleted out-of-band; drop the resource so Terraform plans a recreate.
	resp.State.RemoveResource(ctx)
}

func (r *llmModelAccessResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmModelAccessResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	targets := buildLlmModelAccessTargets(plan.Targets, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	name := plan.Name.ValueString()
	scopeType := plan.ScopeType.ValueString()
	policyType := plan.PolicyType.ValueString()
	body := &llmModelAccessUpdateRequest{
		Name:       &name,
		ScopeType:  &scopeType,
		PolicyType: &policyType,
		Targets:    &targets,
		Enabled:    boolPtrFromBool(plan.Enabled),
		ChangeNote: stringPtrIfKnown(plan.ChangeNote),
	}
	if v, ok := knownString(plan.ScopeID); ok {
		body.ScopeID = &v
	}
	if v, ok := knownString(plan.ScopeValue); ok {
		body.ScopeValue = &v
	}
	if v, ok := knownString(plan.TrafficType); ok {
		body.TrafficType = &v
	}

	var policy llmModelAccessResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/model-access/"+plan.ID.ValueString(), body, &policy); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model-access policy", "update the LLM model-access policy", err)
		return
	}

	newState := applyLlmModelAccessResponse(&policy, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the policy. A 404 means it is already gone — success for a
// destroy.
func (r *llmModelAccessResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmModelAccessResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/model-access/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model-access policy", "delete the LLM model-access policy", err)
	}
}

func (r *llmModelAccessResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmModelAccessResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// --- shared scope/traffic schema helpers -------------------------------------

// llmScopeTypeAttribute builds the scope_type attribute shared by the
// governance policy resources. requiresReplace marks scope changes as
// replacements (token budgets — their update API cannot change the scope).
func llmScopeTypeAttribute(scopeTypes []string, requiresReplace bool) schema.StringAttribute {
	attr := schema.StringAttribute{
		MarkdownDescription: "Identity dimension the policy applies to: one of `" +
			joinBackticked(scopeTypes) + "`. `org` applies to everyone; the others select by " +
			"`scope_id` (UUID-keyed scopes) or `scope_value` (name-keyed scopes such as `role`/`group`).",
		Required: true,
		Validators: []validator.String{
			stringvalidator.OneOf(scopeTypes...),
		},
	}
	if requiresReplace {
		attr.MarkdownDescription += " Changing it forces a new resource (the API has no update for it)."
		attr.PlanModifiers = []planmodifier.String{stringplanmodifier.RequiresReplace()}
	}
	return attr
}

// llmScopeIDAttribute builds the scope_id attribute shared by the governance
// policy resources. The update APIs COALESCE it and cannot clear it, so
// removing it forces a new resource.
func llmScopeIDAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "UUID of the scoped entity (user, team, API key, …) for UUID-keyed scope " +
			"types. Removing it forces a new resource — the API cannot clear it in place.",
		Optional: true,
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.RequiresReplaceIf(llmReplaceWhenCleared,
				"Removing scope_id requires replacement: the API cannot clear it in place.",
				"Removing `scope_id` requires replacement: the API cannot clear it in place."),
		},
	}
}

// llmScopeValueAttribute builds the scope_value attribute shared by the
// governance policy resources; hint names example scope values.
func llmScopeValueAttribute(hint string) schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "String key of the scoped entity for name-keyed scope types (" + hint + "). " +
			"Removing it forces a new resource — the API cannot clear it in place.",
		Optional: true,
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.RequiresReplaceIf(llmReplaceWhenCleared,
				"Removing scope_value requires replacement: the API cannot clear it in place.",
				"Removing `scope_value` requires replacement: the API cannot clear it in place."),
		},
	}
}

// llmReplaceWhenCleared requires replacement only when a previously-set
// value is removed from configuration: the governance update APIs COALESCE
// these columns, so a value can be changed in place but never cleared.
func llmReplaceWhenCleared(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	if req.ConfigValue.IsNull() && !req.StateValue.IsNull() {
		resp.RequiresReplace = true
	}
}

// llmMemberOfGroupAttribute builds the member_of_group attribute shared by
// the token-budget and rate-limit resources; what names the resource kind
// ("budget", "rate limit"). Neither update API accepts the field, so it is
// create-only: a change forces replacement rather than a PUT the platform
// would silently ignore (reporting success while the next plan shows the same
// diff).
func llmMemberOfGroupAttribute(what string) schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "IdP group name that narrows a broad per-user " + what + " to the members of " +
			"that group. Each member gets their **own, independent** allowance from this single rule, and a " +
			"user who joins the group picks it up on their next request with no admin action. This is the " +
			"opposite of `scope_type = \"group\"` (with the group name in `scope_value`), which is **one " +
			"allowance pooled** across every member, so the first heavy user can exhaust it for everyone.\n\n" +
			"Only valid with `scope_type = \"user\"` and neither `scope_id` nor `scope_value` set. Changing " +
			"it forces a new " + what + " (the API cannot update it).",
		Optional: true,
		Validators: []validator.String{
			noSurroundingWhitespace,
		},
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.RequiresReplace(),
		},
	}
}

// llmMemberOfGroupShapeValidator mirrors the platform's member_of_group shape
// rule (the V65–V67 CHECKs and `validate_member_of_group_shape`): the filter
// only means something on a broad per-user rule, whose counter is already
// keyed per user and which does not name one entity. Checking it at plan time
// turns an apply-time 400 into a plan error, and — because member_of_group
// forces replacement — also stops an in-place scope change on a filtered rate
// limit, which the platform rejects.
type llmMemberOfGroupShapeValidator struct {
	what string
}

func (v llmMemberOfGroupShapeValidator) Description(_ context.Context) string {
	return "member_of_group requires scope_type = \"user\" with neither scope_id nor scope_value set"
}

func (v llmMemberOfGroupShapeValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v llmMemberOfGroupShapeValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var group, scopeType, scopeID, scopeValue types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("member_of_group"), &group)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("scope_type"), &scopeType)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("scope_id"), &scopeID)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("scope_value"), &scopeValue)...)
	if resp.Diagnostics.HasError() || group.IsNull() || group.IsUnknown() {
		return
	}

	attr := path.Root("member_of_group")
	if st, ok := knownString(scopeType); ok && st != "user" {
		resp.Diagnostics.AddAttributeError(attr, "member_of_group requires scope_type = \"user\"",
			fmt.Sprintf("member_of_group gives each member of the group their own %s, which only works "+
				"where the counter is keyed per user. For one %s pooled across the group, use "+
				"scope_type = \"group\" with the group name in scope_value instead.", v.what, v.what))
	}
	if _, ok := knownString(scopeID); ok {
		resp.Diagnostics.AddAttributeError(attr, "member_of_group cannot be combined with scope_id",
			fmt.Sprintf("A %s with scope_id already names one user. Leave scope_id unset to cover every "+
				"member of the group.", v.what))
	}
	if _, ok := knownString(scopeValue); ok {
		resp.Diagnostics.AddAttributeError(attr, "member_of_group cannot be combined with scope_value",
			fmt.Sprintf("A %s with scope_value names one entity, so the platform would treat it as a "+
				"specific override rather than a per-member rule. Leave scope_value unset to cover every "+
				"member of the group.", v.what))
	}
}

// llmTrafficTypeAttribute builds the traffic_type attribute shared by the
// governance policy resources; def names the server default.
func llmTrafficTypeAttribute(defaultValue string, def defaults.String) schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "Traffic lane the policy applies to: `all`, `llm` (model-proxy traffic), or " +
			"`mcp` (MCP tool traffic). Defaults to `" + defaultValue + "`.",
		Optional: true,
		Computed: true,
		Default:  def,
		Validators: []validator.String{
			stringvalidator.OneOf(llmGatewayTrafficTypes...),
		},
	}
}

// llmEnabledAttribute builds the enabled attribute shared by the governance
// policy resources (server default true; the create endpoints have no
// enabled field, so a false plan is converged with a follow-up update).
func llmEnabledAttribute() schema.BoolAttribute {
	return schema.BoolAttribute{
		MarkdownDescription: "Whether the policy is enforced. Defaults to `true`.",
		Optional:            true,
		Computed:            true,
		Default:             booldefault.StaticBool(true),
	}
}

// joinBackticked renders a value list as `a` / `b` / `c` (sans outer ticks —
// callers embed it inside a backticked span).
func joinBackticked(values []string) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += "` / `"
		}
		out += v
	}
	return out
}

// --- request/response DTOs ---------------------------------------------------

// llmModelAccessTargetPayload mirrors the tagged ModelAccessTarget wire shape
// (`{"kind": "...", ...}`).
type llmModelAccessTargetPayload struct {
	Kind       string  `json:"kind"`
	Alias      *string `json:"alias,omitempty"`
	Model      *string `json:"model,omitempty"`
	ProviderID *string `json:"provider_id,omitempty"`
	GroupID    *string `json:"group_id,omitempty"`
}

// llmModelAccessCreateRequest mirrors the llm-gateway CreateModelAccessRequest
// body.
type llmModelAccessCreateRequest struct {
	Name        string                        `json:"name"`
	ScopeType   string                        `json:"scope_type"`
	ScopeID     *string                       `json:"scope_id,omitempty"`
	ScopeValue  *string                       `json:"scope_value,omitempty"`
	PolicyType  string                        `json:"policy_type"`
	Targets     []llmModelAccessTargetPayload `json:"targets"`
	TrafficType *string                       `json:"traffic_type,omitempty"`
	ChangeNote  *string                       `json:"change_note,omitempty"`
}

// llmModelAccessUpdateRequest mirrors the llm-gateway UpdateModelAccessRequest
// body. Omitted keys leave the corresponding column unchanged, so Update sends
// every managed field.
type llmModelAccessUpdateRequest struct {
	Name        *string                        `json:"name,omitempty"`
	ScopeType   *string                        `json:"scope_type,omitempty"`
	ScopeID     *string                        `json:"scope_id,omitempty"`
	ScopeValue  *string                        `json:"scope_value,omitempty"`
	PolicyType  *string                        `json:"policy_type,omitempty"`
	Targets     *[]llmModelAccessTargetPayload `json:"targets,omitempty"`
	TrafficType *string                        `json:"traffic_type,omitempty"`
	Enabled     *bool                          `json:"enabled,omitempty"`
	ChangeNote  *string                        `json:"change_note,omitempty"`
}

// llmModelAccessResponse mirrors the llm-gateway ModelAccessPolicy response.
type llmModelAccessResponse struct {
	ID          string                        `json:"id"`
	OrgID       string                        `json:"org_id"`
	Name        string                        `json:"name"`
	ScopeType   string                        `json:"scope_type"`
	ScopeID     *string                       `json:"scope_id"`
	ScopeValue  *string                       `json:"scope_value"`
	PolicyType  string                        `json:"policy_type"`
	Targets     []llmModelAccessTargetPayload `json:"targets"`
	TrafficType string                        `json:"traffic_type"`
	Enabled     bool                          `json:"enabled"`
}

// buildLlmModelAccessTargets converts the planned targets to the API shape,
// validating that each entry carries exactly the fields its kind requires —
// a mismatched shape would otherwise surface as an opaque deserialization
// error from the API. ValidateConfig runs the same check at plan time; this
// one also covers values that were unknown then.
func buildLlmModelAccessTargets(targets []llmModelAccessTargetModel, diags *diag.Diagnostics) []llmModelAccessTargetPayload {
	out := make([]llmModelAccessTargetPayload, 0, len(targets))
	for i, t := range targets {
		if !checkLlmModelAccessTargetShape(i, t, func(v types.String) bool {
			_, ok := knownString(v)
			return ok
		}, diags) {
			continue
		}

		payload := llmModelAccessTargetPayload{Kind: t.Kind.ValueString()}
		if v, ok := knownString(t.Alias); ok {
			payload.Alias = &v
		}
		if v, ok := knownString(t.Model); ok {
			payload.Model = &v
		}
		if v, ok := knownString(t.ProviderID); ok {
			payload.ProviderID = &v
		}
		if v, ok := knownString(t.GroupID); ok {
			payload.GroupID = &v
		}
		out = append(out, payload)
	}
	return out
}

// llmModelAccessTargetFields lists, per target kind, which of alias / model /
// provider_id / group_id the kind requires; every other field is forbidden.
var llmModelAccessTargetFields = map[string][4]bool{
	"model_alias":    {true, false, false, false},
	"model":          {false, true, false, false},
	"provider":       {false, false, true, false},
	"provider_model": {false, true, true, false},
	"route_group":    {false, false, false, true},
}

// checkLlmModelAccessTargetShape reports, against targets[index], a target
// whose fields do not match its kind. isSet decides whether a field counts as
// present. An unrecognized kind is left to the kind attribute's validator.
func checkLlmModelAccessTargetShape(index int, t llmModelAccessTargetModel, isSet func(types.String) bool, diags *diag.Diagnostics) bool {
	kind := t.Kind.ValueString()
	want, known := llmModelAccessTargetFields[kind]
	if !known {
		return true
	}
	have := [4]bool{isSet(t.Alias), isSet(t.Model), isSet(t.ProviderID), isSet(t.GroupID)}
	if have != want {
		diags.AddAttributeError(
			path.Root("targets").AtListIndex(index),
			"Invalid model-access target shape",
			fmt.Sprintf("kind = %q requires %s", kind, llmTargetShapeHint(kind)),
		)
		return false
	}
	return true
}

// llmTargetShapeHint names the fields a target kind requires.
func llmTargetShapeHint(kind string) string {
	switch kind {
	case "model_alias":
		return "`alias` (and none of `model`, `provider_id` or `group_id`)"
	case "model":
		return "`model` (and none of `alias`, `provider_id` or `group_id`)"
	case "provider":
		return "`provider_id` (and none of `alias`, `model` or `group_id`)"
	case "route_group":
		return "`group_id` (and none of `alias`, `model` or `provider_id`)"
	default:
		return "`provider_id` and `model` (and neither `alias` nor `group_id`)"
	}
}

// applyLlmModelAccessResponse maps the server's view onto a state model.
// prior is the plan (Create/Update) or previous state (Read), used to settle
// cleared optionals back to null.
func applyLlmModelAccessResponse(policy *llmModelAccessResponse, prior *llmModelAccessResourceModel) llmModelAccessResourceModel {
	targets := make([]llmModelAccessTargetModel, 0, len(policy.Targets))
	for _, t := range policy.Targets {
		targets = append(targets, llmModelAccessTargetModel{
			Kind:       types.StringValue(t.Kind),
			Alias:      stringFromPtr(t.Alias),
			Model:      stringFromPtr(t.Model),
			ProviderID: stringFromPtr(t.ProviderID),
			GroupID:    stringFromPtr(t.GroupID),
		})
	}

	return llmModelAccessResourceModel{
		ID:          types.StringValue(policy.ID),
		OrgID:       types.StringValue(policy.OrgID),
		Name:        types.StringValue(policy.Name),
		ScopeType:   types.StringValue(policy.ScopeType),
		ScopeID:     optionalStringFromPtr(policy.ScopeID, prior.ScopeID),
		ScopeValue:  optionalStringFromPtr(policy.ScopeValue, prior.ScopeValue),
		PolicyType:  types.StringValue(policy.PolicyType),
		Targets:     targets,
		TrafficType: types.StringValue(policy.TrafficType),
		Enabled:     types.BoolValue(policy.Enabled),
		ChangeNote:  prior.ChangeNote, // write-only; see llmChangeNoteAttribute
	}
}

// stringFromPtr maps an optional wire string to state: nil means null.
func stringFromPtr(p *string) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(*p)
}
