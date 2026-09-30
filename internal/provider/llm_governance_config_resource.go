// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
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

// llmDefaultModelAccessPostures are the DefaultModelAccess enum's wire values
// (V70, BCP-3887).
var llmDefaultModelAccessPostures = []string{"allow", "deny"}

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
	DefaultModelAccess        types.String `tfsdk:"default_model_access"`
	RequireRoutingPolicy      types.Bool   `tfsdk:"require_routing_policy"`
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
			"The platform stores every setting in one row and its update replaces the whole row, so " +
			"each apply first reads the current configuration and changes only what this resource " +
			"configures. Settings left unconfigured keep whatever the app last set.\n\n" +
			"`terraform destroy` cannot delete the configuration. It **resets " +
			"`require_pricing_for_mappings`** to the platform default (`" +
			fmt.Sprintf("%t", llmGovernanceConfigDefaultRequirePricing) + "`), then forgets the " +
			"resource. `default_model_access` and `require_routing_policy` are left as they are, so " +
			"destroying this resource never loosens model access.",
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
			"default_model_access": schema.StringAttribute{
				MarkdownDescription: "What happens to a request for a model that no model-access policy " +
					"mentions: `allow` (the platform default) or `deny`. Setting `deny` requires at least one " +
					"enabled allowlist, so switching over cannot lock the organization out; the API rejects " +
					"it otherwise. Allowlists narrow access and never grant it on their own, so `deny` is " +
					"what makes them a closed list. Keeps the stored value when unset.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.OneOf(llmDefaultModelAccessPostures...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"require_routing_policy": schema.BoolAttribute{
				MarkdownDescription: "Whether callers must address a routing policy rather than naming a " +
					"model directly on the chat surfaces. `/v1/systemone` (TypeSafe Jev) is exempt, because a " +
					"routing policy only chooses between chat models. Model access, budgets, rate limits and " +
					"DLP still apply there. The platform default is `false`. Keeps the stored value when unset.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
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
	cfg.applyTo(&state)
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

// Delete resets require_pricing_for_mappings to the platform default (the API
// has no delete endpoint), then lets Terraform forget the resource. The other
// settings are carried through unchanged: resetting default_model_access
// would flip a deny-by-default organization back to allow as a side effect of
// removing configuration.
func (r *llmGovernanceConfigResource) Delete(ctx context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	r.readModifyWrite(ctx, map[string]any{
		"require_pricing_for_mappings": llmGovernanceConfigDefaultRequirePricing,
	}, "reset the LLM governance settings to the platform defaults (destroy)", &resp.Diagnostics)
}

// ImportState imports the organization's singleton configuration. The API
// resolves the organization from the credential's token claims, so the import
// ID is not looked up — pass the organization ID for consistency; Read
// replaces it with the authoritative value either way.
func (r *llmGovernanceConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// put writes the planned configuration through the upsert endpoint and stamps
// the singleton's identity and the server's view onto the plan model. Unknown
// (unconfigured, first apply) settings are not sent, so they keep their
// current value.
func (r *llmGovernanceConfigResource) put(ctx context.Context, plan *llmGovernanceConfigResourceModel, action string, diags *diag.Diagnostics) {
	changes := map[string]any{
		"require_pricing_for_mappings": plan.RequirePricingForMappings.ValueBool(),
	}
	if v, ok := knownString(plan.DefaultModelAccess); ok {
		changes["default_model_access"] = v
	}
	if v, ok := knownBool(plan.RequireRoutingPolicy); ok {
		changes["require_routing_policy"] = v
	}

	cfg, ok := r.readModifyWrite(ctx, changes, action, diags)
	if !ok {
		return
	}
	plan.ID = types.StringValue(r.client.OrganizationID())
	cfg.applyTo(plan)
}

// readModifyWrite overlays changes on the current configuration and PUTs the
// result. The endpoint replaces the whole row, and a field missing from the
// body takes its serde default rather than keeping its value. Starting from
// the raw GET body preserves every field this provider does not model,
// including ones added by later platform versions.
func (r *llmGovernanceConfigResource) readModifyWrite(ctx context.Context, changes map[string]any, action string, diags *diag.Diagnostics) (llmGovernanceConfigPayload, bool) {
	var current map[string]json.RawMessage
	if err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/governance-config", nil, &current); err != nil {
		addLlmGatewayAPIError(diags, "LLM governance configuration", action, err)
		return llmGovernanceConfigPayload{}, false
	}

	body := make(map[string]any, len(current)+len(changes))
	for k, v := range current {
		body[k] = v
	}
	for k, v := range changes {
		body[k] = v
	}

	var cfg llmGovernanceConfigPayload
	if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/governance-config", body, &cfg); err != nil {
		addLlmGatewayAPIError(diags, "LLM governance configuration", action, err)
		return llmGovernanceConfigPayload{}, false
	}
	return cfg, true
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

// llmGovernanceConfigPayload mirrors the llm-gateway GovernanceConfig
// response. Requests are built by readModifyWrite instead: the endpoint has
// no partial-update semantics, and a field left out of the body is reset to
// its default rather than kept.
type llmGovernanceConfigPayload struct {
	RequirePricingForMappings bool   `json:"require_pricing_for_mappings"`
	DefaultModelAccess        string `json:"default_model_access"`
	RequireRoutingPolicy      bool   `json:"require_routing_policy"`
}

// applyTo maps the server's view onto a state model. A platform too old to
// report default_model_access has only the allow posture.
func (cfg llmGovernanceConfigPayload) applyTo(m *llmGovernanceConfigResourceModel) {
	posture := cfg.DefaultModelAccess
	if posture == "" {
		posture = "allow"
	}
	m.RequirePricingForMappings = types.BoolValue(cfg.RequirePricingForMappings)
	m.DefaultModelAccess = types.StringValue(posture)
	m.RequireRoutingPolicy = types.BoolValue(cfg.RequireRoutingPolicy)
}
