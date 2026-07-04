// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                     = &llmRateLimitResource{}
	_ resource.ResourceWithConfigure        = &llmRateLimitResource{}
	_ resource.ResourceWithImportState      = &llmRateLimitResource{}
	_ resource.ResourceWithConfigValidators = &llmRateLimitResource{}
)

// NewLlmRateLimitResource returns a new barndoor_llm_rate_limit resource.
func NewLlmRateLimitResource() resource.Resource {
	return &llmRateLimitResource{}
}

// llmRateLimitResource manages an LLM Gateway rate-limit policy through the
// llm-gateway-service admin REST API (`/api/llm-gateway/admin/rate-limits`).
type llmRateLimitResource struct {
	client *client.Client
}

// llmRateLimitResourceModel maps the resource schema to Go types.
type llmRateLimitResourceModel struct {
	ID                types.String `tfsdk:"id"`
	OrgID             types.String `tfsdk:"org_id"`
	Name              types.String `tfsdk:"name"`
	ScopeType         types.String `tfsdk:"scope_type"`
	ScopeID           types.String `tfsdk:"scope_id"`
	ScopeValue        types.String `tfsdk:"scope_value"`
	RequestsPerMinute types.Int64  `tfsdk:"requests_per_minute"`
	TokensPerMinute   types.Int64  `tfsdk:"tokens_per_minute"`
	TrafficType       types.String `tfsdk:"traffic_type"`
	Enabled           types.Bool   `tfsdk:"enabled"`
}

func (r *llmRateLimitResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_rate_limit"
}

func (r *llmRateLimitResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway rate-limit policy: a per-minute request and/or " +
			"token ceiling applied to an identity scope (the organization, a user, an IdP group, …).\n\n" +
			"At least one of `requests_per_minute` / `tokens_per_minute` must be set; removing one from " +
			"configuration clears that metric on the platform. The platform allows one policy per " +
			"`(scope, traffic_type)` combination and answers 409 on duplicates.",
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
			"scope_type": llmScopeTypeAttribute(llmGatewayRateLimitScopeTypes, false),
			"scope_id":   llmScopeIDAttribute(),
			"scope_value": llmScopeValueAttribute("e.g. a role or IdP group name for `role`/`group` " +
				"scopes"),
			"requests_per_minute": schema.Int64Attribute{
				MarkdownDescription: "Requests allowed per rolling 60-second window. Omit to enforce " +
					"tokens only.",
				Optional: true,
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
			},
			"tokens_per_minute": schema.Int64Attribute{
				MarkdownDescription: "Tokens allowed per rolling 60-second window. Omit to enforce " +
					"requests only.",
				Optional: true,
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
			},
			"traffic_type": llmTrafficTypeAttribute("all", stringdefault.StaticString("all")),
			"enabled":      llmEnabledAttribute(),
		},
	}
}

// ConfigValidators enforces the API invariant that a policy carries at least
// one metric.
func (r *llmRateLimitResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.AtLeastOneOf(
			path.MatchRoot("requests_per_minute"),
			path.MatchRoot("tokens_per_minute"),
		),
	}
}

func (r *llmRateLimitResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmRateLimitResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmRateLimitResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := &llmRateLimitCreateRequest{
		Name:              plan.Name.ValueString(),
		ScopeType:         plan.ScopeType.ValueString(),
		RequestsPerMinute: int32PtrFromInt64(plan.RequestsPerMinute),
		TokensPerMinute:   int32PtrFromInt64(plan.TokensPerMinute),
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

	var policy llmRateLimitResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/rate-limits", body, &policy); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM rate-limit policy", "create the LLM rate-limit policy", err)
		return
	}
	if policy.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no policy id.")
		return
	}

	// Record the created policy before any follow-up call so a failure below
	// still leaves it tracked rather than orphaned.
	state := applyLlmRateLimitResponse(&policy, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The create endpoint has no `enabled` field (new policies are always
	// enabled); converge an `enabled = false` plan with an immediate update.
	if enabled, ok := knownBool(plan.Enabled); ok && !enabled {
		disable := false
		if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/rate-limits/"+policy.ID,
			&llmRateLimitUpdateRequest{Enabled: &disable}, &policy); err != nil {
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM rate-limit policy",
				"disable the LLM rate-limit policy after create", err)
			return
		}
		state = applyLlmRateLimitResponse(&policy, &plan)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	}
}

// Read refreshes the policy from the org-wide listing: the admin API has no
// get-by-id endpoint for rate-limit policies.
func (r *llmRateLimitResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmRateLimitResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policies []llmRateLimitResponse
	if err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/rate-limits", nil, &policies); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM rate-limit policy", "list LLM rate-limit policies", err)
		return
	}

	for i := range policies {
		if policies[i].ID == state.ID.ValueString() {
			newState := applyLlmRateLimitResponse(&policies[i], &state)
			resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
			return
		}
	}

	// Deleted out-of-band; drop the resource so Terraform plans a recreate.
	resp.State.RemoveResource(ctx)
}

func (r *llmRateLimitResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmRateLimitResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := plan.Name.ValueString()
	scopeType := plan.ScopeType.ValueString()
	body := &llmRateLimitUpdateRequest{
		Name:      &name,
		ScopeType: &scopeType,
		// Both metrics are always present in the body: the API's tri-state
		// PATCH semantics treat an explicit JSON null as "clear this metric",
		// which is exactly what a removed configuration value means.
		RequestsPerMinute: int32PtrFromInt64(plan.RequestsPerMinute),
		TokensPerMinute:   int32PtrFromInt64(plan.TokensPerMinute),
		Enabled:           boolPtrFromBool(plan.Enabled),
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

	var policy llmRateLimitResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/rate-limits/"+plan.ID.ValueString(), body, &policy); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM rate-limit policy", "update the LLM rate-limit policy", err)
		return
	}

	newState := applyLlmRateLimitResponse(&policy, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the policy. A 404 means it is already gone — success for a
// destroy.
func (r *llmRateLimitResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmRateLimitResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/rate-limits/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM rate-limit policy", "delete the LLM rate-limit policy", err)
	}
}

func (r *llmRateLimitResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmRateLimitResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// --- request/response DTOs ---------------------------------------------------

// llmRateLimitCreateRequest mirrors the llm-gateway CreateRateLimitRequest
// body.
type llmRateLimitCreateRequest struct {
	Name              string  `json:"name"`
	ScopeType         string  `json:"scope_type"`
	ScopeID           *string `json:"scope_id,omitempty"`
	ScopeValue        *string `json:"scope_value,omitempty"`
	RequestsPerMinute *int32  `json:"requests_per_minute,omitempty"`
	TokensPerMinute   *int32  `json:"tokens_per_minute,omitempty"`
	TrafficType       *string `json:"traffic_type,omitempty"`
}

// llmRateLimitUpdateRequest mirrors the llm-gateway UpdateRateLimitRequest
// body. The two metric keys are deliberately **not** omitempty: the API's
// tri-state PATCH semantics distinguish an absent key (keep the current
// value) from an explicit null (clear the metric), and Terraform's plan is
// the full desired state — a nil pointer must clear.
type llmRateLimitUpdateRequest struct {
	Name              *string `json:"name,omitempty"`
	ScopeType         *string `json:"scope_type,omitempty"`
	ScopeID           *string `json:"scope_id,omitempty"`
	ScopeValue        *string `json:"scope_value,omitempty"`
	RequestsPerMinute *int32  `json:"requests_per_minute"`
	TokensPerMinute   *int32  `json:"tokens_per_minute"`
	TrafficType       *string `json:"traffic_type,omitempty"`
	Enabled           *bool   `json:"enabled,omitempty"`
}

// llmRateLimitResponse mirrors the ai_governance RateLimitPolicy response.
type llmRateLimitResponse struct {
	ID                string  `json:"id"`
	OrgID             string  `json:"org_id"`
	Name              string  `json:"name"`
	ScopeType         string  `json:"scope_type"`
	ScopeID           *string `json:"scope_id"`
	ScopeValue        *string `json:"scope_value"`
	RequestsPerMinute *int32  `json:"requests_per_minute"`
	TokensPerMinute   *int32  `json:"tokens_per_minute"`
	TrafficType       string  `json:"traffic_type"`
	Enabled           bool    `json:"enabled"`
}

// applyLlmRateLimitResponse maps the server's view onto a state model. prior
// is the plan (Create/Update) or previous state (Read), used to settle
// cleared optionals back to null.
func applyLlmRateLimitResponse(policy *llmRateLimitResponse, prior *llmRateLimitResourceModel) llmRateLimitResourceModel {
	return llmRateLimitResourceModel{
		ID:                types.StringValue(policy.ID),
		OrgID:             types.StringValue(policy.OrgID),
		Name:              types.StringValue(policy.Name),
		ScopeType:         types.StringValue(policy.ScopeType),
		ScopeID:           optionalStringFromPtr(policy.ScopeID, prior.ScopeID),
		ScopeValue:        optionalStringFromPtr(policy.ScopeValue, prior.ScopeValue),
		RequestsPerMinute: int64FromInt32Ptr(policy.RequestsPerMinute),
		TokensPerMinute:   int64FromInt32Ptr(policy.TokensPerMinute),
		TrafficType:       types.StringValue(policy.TrafficType),
		Enabled:           types.BoolValue(policy.Enabled),
	}
}
