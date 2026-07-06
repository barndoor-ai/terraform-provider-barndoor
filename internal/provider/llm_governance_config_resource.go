// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmGovernanceConfigDefaultRequirePricing is the platform default for
// `require_pricing_for_mappings`: the llm_gw.governance_config column
// default (bdai-platform migration V10__governance_config.sql), which is also
// what the API reports when the org has no configuration row at all. Delete
// writes this back before forgetting the resource, so a destroyed
// configuration behaves as if it was never managed.
const llmGovernanceConfigDefaultRequirePricing = false

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &llmGovernanceConfigResource{}
	_ resource.ResourceWithConfigure   = &llmGovernanceConfigResource{}
	_ resource.ResourceWithImportState = &llmGovernanceConfigResource{}
)

// NewLlmGovernanceConfigResource returns a new barndoor_llm_governance_config
// resource.
func NewLlmGovernanceConfigResource() resource.Resource {
	return &llmGovernanceConfigResource{}
}

// llmGovernanceConfigResource manages the organization's singleton LLM
// Gateway governance configuration through the llm-gateway-service admin REST
// API (`/api/llm-gateway/admin/governance-config`).
type llmGovernanceConfigResource struct {
	client *client.Client
}

// llmGovernanceConfigResourceModel maps the resource schema to Go types.
type llmGovernanceConfigResourceModel struct {
	ID                        types.String `tfsdk:"id"`
	RequirePricingForMappings types.Bool   `tfsdk:"require_pricing_for_mappings"`
}

func (r *llmGovernanceConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_governance_config"
}

func (r *llmGovernanceConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the organization's **singleton** LLM Gateway governance " +
			"configuration. The platform keys this configuration by the organization (an org without a " +
			"configuration row behaves as all-defaults), so this resource **adopts and configures** the " +
			"singleton rather than creating anything.\n\n" +
			"`terraform destroy` cannot delete the configuration — it **resets every setting to the " +
			"platform defaults** (`require_pricing_for_mappings = " +
			fmt.Sprintf("%t", llmGovernanceConfigDefaultRequirePricing) + "`) and then forgets the " +
			"resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "ID of the configuration; the API keys the singleton by the " +
					"credential's organization, so this equals the provider's `organization_id`.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"require_pricing_for_mappings": schema.BoolAttribute{
				MarkdownDescription: "Whether every model mapping (route) must have a matching pricing " +
					"rule before the gateway accepts it. The platform default is `" +
					fmt.Sprintf("%t", llmGovernanceConfigDefaultRequirePricing) + "`.",
				Required: true,
			},
		},
	}
}

func (r *llmGovernanceConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// Create adopts the organization's governance configuration: the API upserts
// on PUT, so "create" is a configure of the existing singleton.
func (r *llmGovernanceConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmGovernanceConfigResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.put(ctx, &plan, "configure the LLM governance settings", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *llmGovernanceConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmGovernanceConfigResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var cfg llmGovernanceConfigPayload
	if err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/governance-config", nil, &cfg); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM governance configuration",
			"read the LLM governance settings", err)
		return
	}

	state.ID = types.StringValue(r.client.OrganizationID())
	state.RequirePricingForMappings = types.BoolValue(cfg.RequirePricingForMappings)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *llmGovernanceConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmGovernanceConfigResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.put(ctx, &plan, "update the LLM governance settings", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete resets the singleton to the platform defaults (the API has no delete
// endpoint — an all-defaults configuration is indistinguishable from no
// configuration), then lets Terraform forget the resource.
func (r *llmGovernanceConfigResource) Delete(ctx context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	body := &llmGovernanceConfigPayload{
		RequirePricingForMappings: llmGovernanceConfigDefaultRequirePricing,
	}
	if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/governance-config", body, nil); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM governance configuration",
			"reset the LLM governance settings to the platform defaults (destroy)", err)
	}
}

// ImportState imports the organization's singleton configuration. The API
// resolves the organization from the credential's token claims, so the import
// ID is not looked up — pass the organization ID for consistency; Read
// replaces it with the authoritative value either way.
func (r *llmGovernanceConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// put writes the planned configuration through the upsert endpoint and stamps
// the singleton's identity onto the plan model.
func (r *llmGovernanceConfigResource) put(ctx context.Context, plan *llmGovernanceConfigResourceModel, action string, diags *diag.Diagnostics) {
	body := &llmGovernanceConfigPayload{
		RequirePricingForMappings: plan.RequirePricingForMappings.ValueBool(),
	}
	var cfg llmGovernanceConfigPayload
	if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/governance-config", body, &cfg); err != nil {
		addLlmGatewayAPIError(diags, "LLM governance configuration", action, err)
		return
	}

	plan.ID = types.StringValue(r.client.OrganizationID())
	plan.RequirePricingForMappings = types.BoolValue(cfg.RequirePricingForMappings)
}

func (r *llmGovernanceConfigResource) requireClient(diags *diag.Diagnostics) bool {
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

// llmGovernanceConfigPayload mirrors the llm-gateway GovernanceConfig struct,
// which is both the GET/PUT response and the full PUT request body (the
// endpoint has no partial-update semantics: every field is required).
type llmGovernanceConfigPayload struct {
	RequirePricingForMappings bool `json:"require_pricing_for_mappings"`
}
