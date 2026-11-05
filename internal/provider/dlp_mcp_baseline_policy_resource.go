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

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &dlpMcpBaselinePolicyResource{}
	_ resource.ResourceWithConfigure      = &dlpMcpBaselinePolicyResource{}
	_ resource.ResourceWithValidateConfig = &dlpMcpBaselinePolicyResource{}
	_ resource.ResourceWithImportState    = &dlpMcpBaselinePolicyResource{}
)

const dlpMcpBaselinePath = dlpAPIPrefix + "/mcp-baseline-policy"

// NewDlpMcpBaselinePolicyResource returns a new
// barndoor_dlp_mcp_baseline_policy resource.
func NewDlpMcpBaselinePolicyResource() resource.Resource {
	return &dlpMcpBaselinePolicyResource{}
}

// dlpMcpBaselinePolicyResource manages the organization's singleton Baseline
// MCP policy through `/api/dlp/admin/v1/mcp-baseline-policy`. The API has no
// DELETE for it: create and update both PUT the rule set, and destroy PUTs an
// empty one.
type dlpMcpBaselinePolicyResource struct {
	client *client.Client
}

type dlpMcpBaselinePolicyResourceModel struct {
	ID        types.String      `tfsdk:"id"`
	Rules     []dlpMcpRuleModel `tfsdk:"rules"`
	CreatedBy types.String      `tfsdk:"created_by"`
	UpdatedBy types.String      `tfsdk:"updated_by"`
	CreatedAt types.String      `tfsdk:"created_at"`
	UpdatedAt types.String      `tfsdk:"updated_at"`
}

// dlpMcpBaselineResponse mirrors the API's McpBaselinePolicyResponse. Policy
// is nil until the Baseline policy's first save.
type dlpMcpBaselineResponse struct {
	PolicyModel string                `json:"policy_model"`
	Policy      *dlpMcpPolicyResponse `json:"policy"`
}

type dlpMcpBaselinePutRequest struct {
	Rules []dlpMcpRulePayload `json:"rules"`
}

func (r *dlpMcpBaselinePolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_mcp_baseline_policy"
}

func (r *dlpMcpBaselinePolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the organization's Data Protection (DLP) Baseline MCP policy: the rules " +
			"for every MCP server that no `barndoor_dlp_mcp_policy` covers.\n\n" +
			"The Baseline policy is a singleton; declare at most one of these per organization. Creating " +
			"the resource takes over the existing Baseline policy and replaces its rules with the " +
			"configured ones. The Baseline policy can't be deleted, so `terraform destroy` clears its rules " +
			"instead: servers without their own policy are then unprotected.\n\n" +
			"The organization must be on MCP server policies. While it still manages MCP traffic through " +
			"`barndoor_dlp_enforcement_policy`, this API is read-only and writes fail with a 409.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "UUID of the Baseline policy, assigned by the API on its first save.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"rules": dlpMcpRulesAttribute("Rules applied to every MCP server without its own policy, in order. " +
				"Use `[]` for no rules. At most 50."),
			"created_by": schema.StringAttribute{
				MarkdownDescription: "Subject that first saved the Baseline policy (empty when the platform " +
					"could not attribute it).",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_by": schema.StringAttribute{
				MarkdownDescription: "Subject that last updated the Baseline policy.",
				Computed:            true,
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the Baseline policy was first saved (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the Baseline policy was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

func (r *dlpMcpBaselinePolicyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	validateDlpMcpRules(ctx, req.Config, &resp.Diagnostics)
}

func (r *dlpMcpBaselinePolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *dlpMcpBaselinePolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dlpMcpBaselinePolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, ok := r.put(ctx, &plan, "save the Baseline MCP policy", &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dlpMcpBaselinePolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireDlpClient(r.client, &resp.Diagnostics) {
		return
	}

	var state dlpMcpBaselinePolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var baseline dlpMcpBaselineResponse
	if err := doJSON(ctx, r.client, http.MethodGet, dlpMcpBaselinePath, nil, &baseline); err != nil {
		addDlpMcpPolicyAPIError(&resp.Diagnostics, "read the Baseline MCP policy", err)
		return
	}
	if baseline.Policy == nil {
		// Never saved (only possible after an import or a platform-side
		// reset); plan a create.
		resp.State.RemoveResource(ctx)
		return
	}

	newState, err := applyDlpMcpBaselineResponse(ctx, baseline.Policy, &state)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *dlpMcpBaselinePolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan dlpMcpBaselinePolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, ok := r.put(ctx, &plan, "update the Baseline MCP policy", &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete clears the Baseline policy's rules: the API has no route to delete
// the policy itself, and leaving its rules in place would keep enforcing a
// configuration Terraform no longer manages.
func (r *dlpMcpBaselinePolicyResource) Delete(ctx context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !requireDlpClient(r.client, &resp.Diagnostics) {
		return
	}
	body := &dlpMcpBaselinePutRequest{Rules: []dlpMcpRulePayload{}}
	if err := doJSON(ctx, r.client, http.MethodPut, dlpMcpBaselinePath, body, nil); err != nil {
		addDlpMcpPolicyAPIError(&resp.Diagnostics, "clear the Baseline MCP policy's rules (destroy)", err)
	}
}

// ImportState imports the organization's Baseline policy. The API resolves
// the organization from the credential's token claims, so the import ID is not
// looked up (by convention, `baseline`); Read replaces it with the Baseline
// policy's UUID.
func (r *dlpMcpBaselinePolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// put replaces the Baseline policy's rules with the planned ones. The stored
// policy is read first so rules that survive the change are sent back with
// their ids and kept.
func (r *dlpMcpBaselinePolicyResource) put(ctx context.Context, plan *dlpMcpBaselinePolicyResourceModel, action string, diags *diag.Diagnostics) (dlpMcpBaselinePolicyResourceModel, bool) {
	if !requireDlpClient(r.client, diags) {
		return dlpMcpBaselinePolicyResourceModel{}, false
	}

	rules, err := buildDlpMcpRulePayloads(ctx, plan.Rules)
	if err != nil {
		diags.AddError("Invalid Baseline MCP policy configuration", err.Error())
		return dlpMcpBaselinePolicyResourceModel{}, false
	}

	var stored dlpMcpBaselineResponse
	if err := doJSON(ctx, r.client, http.MethodGet, dlpMcpBaselinePath, nil, &stored); err != nil {
		addDlpMcpPolicyAPIError(diags, "read the Baseline MCP policy before saving it", err)
		return dlpMcpBaselinePolicyResourceModel{}, false
	}
	if stored.Policy != nil {
		assignDlpMcpRuleIDs(rules, stored.Policy.Rules)
	}

	var policy dlpMcpPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPut, dlpMcpBaselinePath, &dlpMcpBaselinePutRequest{Rules: rules}, &policy); err != nil {
		addDlpMcpPolicyAPIError(diags, action, err)
		return dlpMcpBaselinePolicyResourceModel{}, false
	}
	if policy.ID == "" {
		diags.AddError("Malformed Data Protection API response", "Saving the Baseline policy returned no policy id.")
		return dlpMcpBaselinePolicyResourceModel{}, false
	}

	state, err := applyDlpMcpBaselineResponse(ctx, &policy, plan)
	if err != nil {
		diags.AddError("Failed to map the Data Protection API response", err.Error())
		return dlpMcpBaselinePolicyResourceModel{}, false
	}
	return state, true
}

func applyDlpMcpBaselineResponse(ctx context.Context, policy *dlpMcpPolicyResponse, prior *dlpMcpBaselinePolicyResourceModel) (dlpMcpBaselinePolicyResourceModel, error) {
	rules, err := applyDlpMcpRules(ctx, policy.Rules, prior.Rules)
	if err != nil {
		return dlpMcpBaselinePolicyResourceModel{}, err
	}
	return dlpMcpBaselinePolicyResourceModel{
		ID:        types.StringValue(policy.ID),
		Rules:     rules,
		CreatedBy: types.StringValue(policy.CreatedBy),
		UpdatedBy: types.StringValue(policy.UpdatedBy),
		CreatedAt: types.StringValue(policy.CreatedAt),
		UpdatedAt: types.StringValue(policy.UpdatedAt),
	}, nil
}
