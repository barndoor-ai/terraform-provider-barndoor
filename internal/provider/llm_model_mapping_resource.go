// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &llmModelMappingResource{}
	_ resource.ResourceWithConfigure   = &llmModelMappingResource{}
	_ resource.ResourceWithImportState = &llmModelMappingResource{}
)

// NewLlmModelMappingResource returns a new barndoor_llm_model_mapping resource.
func NewLlmModelMappingResource() resource.Resource {
	return &llmModelMappingResource{}
}

// llmModelMappingResource manages an LLM Gateway model mapping (a route from
// a caller-facing model alias to an upstream model on a provider) through the
// llm-gateway-service admin REST API (`/api/llm-gateway/admin/model-mappings`).
type llmModelMappingResource struct {
	client *client.Client
}

// llmModelMappingResourceModel maps the resource schema to Go types.
type llmModelMappingResourceModel struct {
	ID                    types.String `tfsdk:"id"`
	ProviderID            types.String `tfsdk:"provider_id"`
	ModelAlias            types.String `tfsdk:"model_alias"`
	UpstreamModel         types.String `tfsdk:"upstream_model"`
	Enabled               types.Bool   `tfsdk:"enabled"`
	Priority              types.Int64  `tfsdk:"priority"`
	RetryOn429Count       types.Int64  `tfsdk:"retry_on_429_count"`
	RetryOn429MaxWaitSecs types.Int64  `tfsdk:"retry_on_429_max_wait_secs"`
	BareAlias             types.Bool   `tfsdk:"bare_alias"`
	StreamIdleTimeoutSecs types.Int64  `tfsdk:"stream_idle_timeout_secs"`
	RequestTimeoutSecs    types.Int64  `tfsdk:"request_timeout_secs"`
}

func (r *llmModelMappingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_model_mapping"
}

func (r *llmModelMappingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway model mapping: the route from a caller-facing " +
			"`model_alias` to an `upstream_model` on a provider, with its failover priority and retry " +
			"policy.\n\n" +
			"Two shapes exist: a **1:1 enablement** (`model_alias == upstream_model`) enables the model " +
			"on the provider, and a **custom alias** (`model_alias != upstream_model`) routes an alias to " +
			"it. A custom alias requires the 1:1 enablement for its `(provider_id, upstream_model)` pair " +
			"to exist first — the API rejects orphan aliases with a 400. The create endpoint **upserts** " +
			"1:1 rows: creating a 1:1 mapping that already exists on the platform adopts and updates the " +
			"existing row instead of failing.\n\n" +
			"`priority` orders failover between routes serving the same alias (lower wins) and is " +
			"written through the per-mapping update endpoint. The platform's bulk " +
			"`PUT /model-mappings/reorder` endpoint is a UI convenience for atomic drag-reordering and " +
			"is not used by this resource — assign each mapping an explicit `priority` instead.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Model mapping UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"provider_id": schema.StringAttribute{
				MarkdownDescription: "UUID of the `barndoor_llm_provider` the mapping routes to. Changing " +
					"it forces a new mapping.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"model_alias": schema.StringAttribute{
				MarkdownDescription: "Caller-facing model name (what API callers put in `model`).",
				Required:            true,
			},
			"upstream_model": schema.StringAttribute{
				MarkdownDescription: "Upstream model the route points at (the provider's model name).",
				Required:            true,
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the route serves traffic. Defaults to `true`. Disabling a " +
					"1:1 enablement row also darkens every custom alias of its upstream model.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"priority": schema.Int64Attribute{
				MarkdownDescription: "Failover order among routes serving the same alias — lower wins. " +
					"Defaults to `0`.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"retry_on_429_count": schema.Int64Attribute{
				MarkdownDescription: "Same-route retries on an upstream 429 before failing over to the " +
					"next route (0–10). `0` (the default) fails over immediately.",
				Optional: true,
				Computed: true,
				Validators: []validator.Int64{
					int64validator.Between(0, 10),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"retry_on_429_max_wait_secs": schema.Int64Attribute{
				MarkdownDescription: "Cap on honoring the upstream `Retry-After` header, in seconds " +
					"(0–180). `0` (the default) uses a small built-in default and ignores the header.",
				Optional: true,
				Computed: true,
				Validators: []validator.Int64{
					int64validator.Between(0, 180),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"bare_alias": schema.BoolAttribute{
				MarkdownDescription: "Whether the row participates in bare-name resolution (a request " +
					"naming the alias alone, without the `<provider>/` prefix). Inferred when unset: " +
					"custom aliases default to `true`, 1:1 enablements to `false`.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"stream_idle_timeout_secs": schema.Int64Attribute{
				MarkdownDescription: "Per-chunk idle timeout for streaming responses, in seconds (1–120). " +
					"The platform default is written when unset.",
				Optional: true,
				Computed: true,
				Validators: []validator.Int64{
					int64validator.Between(1, 120),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"request_timeout_secs": schema.Int64Attribute{
				MarkdownDescription: "Total-request timeout for non-streaming requests, in seconds " +
					"(1–600). The platform default is written when unset. On a 1:1 enablement row this " +
					"sets the model tier consulted by every alias of the model.",
				Optional: true,
				Computed: true,
				Validators: []validator.Int64{
					int64validator.Between(1, 600),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *llmModelMappingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmModelMappingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmModelMappingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := buildLlmModelMappingCreateRequest(&plan)

	var mapping llmModelMappingResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/model-mappings", body, &mapping); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model mapping", "create the LLM model mapping", err)
		return
	}
	if mapping.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no model mapping id.")
		return
	}

	state := applyLlmModelMappingResponse(&mapping)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Read refreshes the mapping from the org-wide listing: the admin API has no
// get-by-id endpoint for model mappings.
func (r *llmModelMappingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmModelMappingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var mappings []llmModelMappingResponse
	if err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/model-mappings", nil, &mappings); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model mapping", "list LLM model mappings", err)
		return
	}

	for i := range mappings {
		if mappings[i].ID == state.ID.ValueString() {
			newState := applyLlmModelMappingResponse(&mappings[i])
			resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
			return
		}
	}

	// Deleted out-of-band; drop the resource so Terraform plans a recreate.
	resp.State.RemoveResource(ctx)
}

func (r *llmModelMappingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmModelMappingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := buildLlmModelMappingUpdateRequest(&plan)

	var mapping llmModelMappingResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/model-mappings/"+plan.ID.ValueString(), body, &mapping); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model mapping", "update the LLM model mapping", err)
		return
	}

	newState := applyLlmModelMappingResponse(&mapping)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the mapping. A 404 means it is already gone — success for a
// destroy.
func (r *llmModelMappingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmModelMappingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/model-mappings/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model mapping", "delete the LLM model mapping", err)
	}
}

func (r *llmModelMappingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmModelMappingResource) requireClient(diags *diag.Diagnostics) bool {
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

// llmModelMappingCreateRequest mirrors the llm-gateway CreateModelMappingRequest
// body.
type llmModelMappingCreateRequest struct {
	ProviderID            string `json:"provider_id"`
	ModelAlias            string `json:"model_alias"`
	UpstreamModel         string `json:"upstream_model"`
	Enabled               *bool  `json:"enabled,omitempty"`
	Priority              *int32 `json:"priority,omitempty"`
	RetryOn429Count       *int32 `json:"retry_on_429_count,omitempty"`
	RetryOn429MaxWaitSecs *int32 `json:"retry_on_429_max_wait_secs,omitempty"`
	BareAlias             *bool  `json:"bare_alias,omitempty"`
	StreamIdleTimeoutSecs *int32 `json:"stream_idle_timeout_secs,omitempty"`
	RequestTimeoutSecs    *int32 `json:"request_timeout_secs,omitempty"`
}

// llmModelMappingUpdateRequest mirrors the llm-gateway UpdateModelMappingRequest
// body. Omitted keys leave the corresponding column unchanged, so Update sends
// every managed field — Terraform's plan is the full desired state (the
// values are always known post-create thanks to UseStateForUnknown).
type llmModelMappingUpdateRequest struct {
	ModelAlias            *string `json:"model_alias,omitempty"`
	UpstreamModel         *string `json:"upstream_model,omitempty"`
	Enabled               *bool   `json:"enabled,omitempty"`
	Priority              *int32  `json:"priority,omitempty"`
	RetryOn429Count       *int32  `json:"retry_on_429_count,omitempty"`
	RetryOn429MaxWaitSecs *int32  `json:"retry_on_429_max_wait_secs,omitempty"`
	BareAlias             *bool   `json:"bare_alias,omitempty"`
	StreamIdleTimeoutSecs *int32  `json:"stream_idle_timeout_secs,omitempty"`
	RequestTimeoutSecs    *int32  `json:"request_timeout_secs,omitempty"`
}

// llmModelMappingResponse mirrors the llm-gateway ModelMapping response. The
// org-wide listing wraps it with annotations (`provider_name`,
// `budget_status`, `route_health_status`) that this resource ignores.
type llmModelMappingResponse struct {
	ID                    string `json:"id"`
	ProviderID            string `json:"provider_id"`
	ModelAlias            string `json:"model_alias"`
	UpstreamModel         string `json:"upstream_model"`
	Enabled               bool   `json:"enabled"`
	Priority              int32  `json:"priority"`
	RetryOn429Count       int32  `json:"retry_on_429_count"`
	RetryOn429MaxWaitSecs int32  `json:"retry_on_429_max_wait_secs"`
	BareAlias             bool   `json:"bare_alias"`
	StreamIdleTimeoutSecs *int32 `json:"stream_idle_timeout_secs"`
	RequestTimeoutSecs    *int32 `json:"request_timeout_secs"`
}

// boolPtrFromBool converts a known types.Bool to a *bool wire value; a
// null/unknown value converts to nil (key omitted).
func boolPtrFromBool(v types.Bool) *bool {
	if b, ok := knownBool(v); ok {
		return &b
	}
	return nil
}

// buildLlmModelMappingCreateRequest converts the planned model to the create
// body. Unset optionals are omitted so the API's defaulting applies (enabled
// true, priority 0, bare-alias inference, platform timeout defaults).
func buildLlmModelMappingCreateRequest(plan *llmModelMappingResourceModel) *llmModelMappingCreateRequest {
	return &llmModelMappingCreateRequest{
		ProviderID:            plan.ProviderID.ValueString(),
		ModelAlias:            plan.ModelAlias.ValueString(),
		UpstreamModel:         plan.UpstreamModel.ValueString(),
		Enabled:               boolPtrFromBool(plan.Enabled),
		Priority:              int32PtrFromInt64(plan.Priority),
		RetryOn429Count:       int32PtrFromInt64(plan.RetryOn429Count),
		RetryOn429MaxWaitSecs: int32PtrFromInt64(plan.RetryOn429MaxWaitSecs),
		BareAlias:             boolPtrFromBool(plan.BareAlias),
		StreamIdleTimeoutSecs: int32PtrFromInt64(plan.StreamIdleTimeoutSecs),
		RequestTimeoutSecs:    int32PtrFromInt64(plan.RequestTimeoutSecs),
	}
}

// buildLlmModelMappingUpdateRequest converts the planned model to the update
// body, carrying the full desired state.
func buildLlmModelMappingUpdateRequest(plan *llmModelMappingResourceModel) *llmModelMappingUpdateRequest {
	modelAlias := plan.ModelAlias.ValueString()
	upstreamModel := plan.UpstreamModel.ValueString()
	return &llmModelMappingUpdateRequest{
		ModelAlias:            &modelAlias,
		UpstreamModel:         &upstreamModel,
		Enabled:               boolPtrFromBool(plan.Enabled),
		Priority:              int32PtrFromInt64(plan.Priority),
		RetryOn429Count:       int32PtrFromInt64(plan.RetryOn429Count),
		RetryOn429MaxWaitSecs: int32PtrFromInt64(plan.RetryOn429MaxWaitSecs),
		BareAlias:             boolPtrFromBool(plan.BareAlias),
		StreamIdleTimeoutSecs: int32PtrFromInt64(plan.StreamIdleTimeoutSecs),
		RequestTimeoutSecs:    int32PtrFromInt64(plan.RequestTimeoutSecs),
	}
}

// applyLlmModelMappingResponse maps the server's view onto a state model.
// Every attribute is authoritative in the response (the server materializes
// defaults at insert), so no null settling against a prior is needed.
func applyLlmModelMappingResponse(mapping *llmModelMappingResponse) llmModelMappingResourceModel {
	return llmModelMappingResourceModel{
		ID:                    types.StringValue(mapping.ID),
		ProviderID:            types.StringValue(mapping.ProviderID),
		ModelAlias:            types.StringValue(mapping.ModelAlias),
		UpstreamModel:         types.StringValue(mapping.UpstreamModel),
		Enabled:               types.BoolValue(mapping.Enabled),
		Priority:              types.Int64Value(int64(mapping.Priority)),
		RetryOn429Count:       types.Int64Value(int64(mapping.RetryOn429Count)),
		RetryOn429MaxWaitSecs: types.Int64Value(int64(mapping.RetryOn429MaxWaitSecs)),
		BareAlias:             types.BoolValue(mapping.BareAlias),
		StreamIdleTimeoutSecs: int64FromInt32Ptr(mapping.StreamIdleTimeoutSecs),
		RequestTimeoutSecs:    int64FromInt32Ptr(mapping.RequestTimeoutSecs),
	}
}
