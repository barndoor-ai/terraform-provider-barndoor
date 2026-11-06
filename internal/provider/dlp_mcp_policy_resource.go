// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &dlpMcpPolicyResource{}
	_ resource.ResourceWithConfigure      = &dlpMcpPolicyResource{}
	_ resource.ResourceWithValidateConfig = &dlpMcpPolicyResource{}
	_ resource.ResourceWithImportState    = &dlpMcpPolicyResource{}
)

// dlpMcpBaselineName is the name the API reserves for the Baseline policy
// (compared case-insensitively).
const dlpMcpBaselineName = "Baseline policy"

// NewDlpMcpPolicyResource returns a new barndoor_dlp_mcp_policy resource.
func NewDlpMcpPolicyResource() resource.Resource {
	return &dlpMcpPolicyResource{}
}

// dlpMcpPolicyResource manages one MCP server policy through the dlp-service
// tenant admin REST API (`/api/dlp/admin/v1/mcp-policies`).
type dlpMcpPolicyResource struct {
	client *client.Client
}

type dlpMcpPolicyResourceModel struct {
	ID           types.String      `tfsdk:"id"`
	Name         types.String      `tfsdk:"name"`
	McpServerIDs types.Set         `tfsdk:"mcp_server_ids"`
	Rules        []dlpMcpRuleModel `tfsdk:"rules"`
	CreatedBy    types.String      `tfsdk:"created_by"`
	UpdatedBy    types.String      `tfsdk:"updated_by"`
	CreatedAt    types.String      `tfsdk:"created_at"`
	UpdatedAt    types.String      `tfsdk:"updated_at"`
}

func (r *dlpMcpPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_mcp_policy"
}

func (r *dlpMcpPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Data Protection (DLP) MCP server policy: the rules that protect tool " +
			"traffic to a set of MCP servers.\n\n" +
			"Each MCP server belongs to at most one policy. Servers that no policy claims follow the " +
			"Baseline policy (`barndoor_dlp_mcp_baseline_policy`), and destroying a policy hands its servers " +
			"back to the Baseline policy.\n\n" +
			"The organization must be on MCP server policies. While it still manages MCP traffic through " +
			"`barndoor_dlp_enforcement_policy`, this API is read-only and writes fail with a 409.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Policy UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Policy name, unique among the organization's MCP policies. " +
					"`Baseline policy` is reserved.",
				Required: true,
				Validators: []validator.String{
					noSurroundingWhitespace,
					dlpMcpNotBaselineName{},
				},
			},
			"mcp_server_ids": schema.SetAttribute{
				MarkdownDescription: "IDs of the MCP servers the policy covers. At least one; a server can " +
					"belong to only one policy. The `*` wildcard is not accepted: the Baseline policy covers " +
					"every server not listed in a policy.",
				ElementType: types.StringType,
				Required:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(
						noSurroundingWhitespace,
						stringvalidator.NoneOf("*"),
					),
				},
			},
			"rules": dlpMcpRulesAttribute("Rules applied to the policy's servers, in order. Use `[]` for a " +
				"policy with no rules, which exempts its servers from the Baseline policy. At most 50."),
			"created_by": schema.StringAttribute{
				MarkdownDescription: "Subject that created the policy (empty when the platform could not " +
					"attribute it).",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_by": schema.StringAttribute{
				MarkdownDescription: "Subject that last updated the policy.",
				Computed:            true,
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the policy was created (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the policy was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

func (r *dlpMcpPolicyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	validateDlpMcpRules(ctx, req.Config, &resp.Diagnostics)
}

func (r *dlpMcpPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *dlpMcpPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireDlpClient(r.client, &resp.Diagnostics) {
		return
	}

	var plan dlpMcpPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildDlpMcpPolicyWriteRequest(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid MCP policy configuration", err.Error())
		return
	}

	var policy dlpMcpPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPost, dlpAPIPrefix+"/mcp-policies", body, &policy); err != nil {
		addDlpMcpPolicyAPIError(&resp.Diagnostics, "create the MCP policy", err)
		return
	}
	if policy.ID == "" {
		resp.Diagnostics.AddError("Malformed Data Protection API response", "Create returned no policy id.")
		return
	}

	state, err := applyDlpMcpPolicyResponse(ctx, &policy, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dlpMcpPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireDlpClient(r.client, &resp.Diagnostics) {
		return
	}

	var state dlpMcpPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policy dlpMcpPolicyResponse
	err := doJSON(ctx, r.client, http.MethodGet, dlpAPIPrefix+"/mcp-policies/"+state.ID.ValueString(), nil, &policy)
	if err != nil {
		if isNotFound(err) {
			// Deleted out-of-band; drop the resource so Terraform plans a
			// recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addDlpMcpPolicyAPIError(&resp.Diagnostics, "read the MCP policy", err)
		return
	}
	if policy.IsBaseline {
		resp.Diagnostics.AddError(
			"This is the Baseline policy",
			fmt.Sprintf("MCP policy %s is the organization's Baseline policy, which "+
				"`barndoor_dlp_mcp_policy` can't manage. Manage it with `barndoor_dlp_mcp_baseline_policy`.",
				policy.ID),
		)
		return
	}

	newState, err := applyDlpMcpPolicyResponse(ctx, &policy, &state)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update sends the full desired state. The stored policy is read first so
// rules that survive the change are sent back with their ids and kept.
func (r *dlpMcpPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !requireDlpClient(r.client, &resp.Diagnostics) {
		return
	}

	var plan dlpMcpPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildDlpMcpPolicyWriteRequest(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid MCP policy configuration", err.Error())
		return
	}

	policyPath := dlpAPIPrefix + "/mcp-policies/" + plan.ID.ValueString()
	var stored dlpMcpPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodGet, policyPath, nil, &stored); err != nil {
		addDlpMcpPolicyAPIError(&resp.Diagnostics, "read the MCP policy before updating it", err)
		return
	}
	assignDlpMcpRuleIDs(body.Rules, stored.Rules)

	var policy dlpMcpPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPut, policyPath, body, &policy); err != nil {
		addDlpMcpPolicyAPIError(&resp.Diagnostics, "update the MCP policy", err)
		return
	}

	newState, err := applyDlpMcpPolicyResponse(ctx, &policy, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the policy; its servers fall back to the Baseline policy. A
// 404 means it is already gone — success for a destroy.
func (r *dlpMcpPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !requireDlpClient(r.client, &resp.Diagnostics) {
		return
	}

	var state dlpMcpPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete, dlpAPIPrefix+"/mcp-policies/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addDlpMcpPolicyAPIError(&resp.Diagnostics, "delete the MCP policy", err)
	}
}

func (r *dlpMcpPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// requireDlpClient reports a diagnostic and false when the provider failed to
// configure its client.
func requireDlpClient(c *client.Client, diags *diag.Diagnostics) bool {
	if c == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// dlpMcpPolicyWriteRequest is the create and update body. Update is a partial
// update on the API side, so every managed field is always sent: Terraform's
// plan is the full desired state.
type dlpMcpPolicyWriteRequest struct {
	Name         string              `json:"name"`
	McpServerIDs []string            `json:"mcp_server_ids"`
	Rules        []dlpMcpRulePayload `json:"rules"`
}

func buildDlpMcpPolicyWriteRequest(ctx context.Context, plan *dlpMcpPolicyResourceModel) (*dlpMcpPolicyWriteRequest, error) {
	servers, err := stringsFromSet(ctx, plan.McpServerIDs)
	if err != nil {
		return nil, fmt.Errorf("mcp_server_ids: %w", err)
	}
	rules, err := buildDlpMcpRulePayloads(ctx, plan.Rules)
	if err != nil {
		return nil, err
	}
	return &dlpMcpPolicyWriteRequest{
		Name:         plan.Name.ValueString(),
		McpServerIDs: nonNilStrings(servers),
		Rules:        rules,
	}, nil
}

func applyDlpMcpPolicyResponse(ctx context.Context, policy *dlpMcpPolicyResponse, prior *dlpMcpPolicyResourceModel) (dlpMcpPolicyResourceModel, error) {
	servers, diags := types.SetValueFrom(ctx, types.StringType, nonNilStrings(policy.McpServerIDs))
	if diags.HasError() {
		return dlpMcpPolicyResourceModel{}, fmt.Errorf("mcp_server_ids: %v", diags)
	}
	rules, err := applyDlpMcpRules(ctx, policy.Rules, prior.Rules)
	if err != nil {
		return dlpMcpPolicyResourceModel{}, err
	}
	return dlpMcpPolicyResourceModel{
		ID:           types.StringValue(policy.ID),
		Name:         types.StringValue(policy.Name),
		McpServerIDs: servers,
		Rules:        rules,
		CreatedBy:    types.StringValue(policy.CreatedBy),
		UpdatedBy:    types.StringValue(policy.UpdatedBy),
		CreatedAt:    types.StringValue(policy.CreatedAt),
		UpdatedAt:    types.StringValue(policy.UpdatedAt),
	}, nil
}

// dlpMcpNotBaselineName rejects the name the API reserves for the Baseline
// policy, matching the API's case-insensitive check.
type dlpMcpNotBaselineName struct{}

func (dlpMcpNotBaselineName) Description(_ context.Context) string {
	return fmt.Sprintf("must not be %q", dlpMcpBaselineName)
}

func (v dlpMcpNotBaselineName) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (dlpMcpNotBaselineName) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if strings.EqualFold(strings.TrimSpace(req.ConfigValue.ValueString()), dlpMcpBaselineName) {
		resp.Diagnostics.AddAttributeError(req.Path, "Reserved policy name",
			fmt.Sprintf("%q is reserved for the Baseline policy. Choose another name, or manage the Baseline "+
				"policy with `barndoor_dlp_mcp_baseline_policy`.", dlpMcpBaselineName))
	}
}
