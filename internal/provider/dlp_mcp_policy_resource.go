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
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &dlpMcpPolicyResource{}
	_ resource.ResourceWithConfigure      = &dlpMcpPolicyResource{}
	_ resource.ResourceWithValidateConfig = &dlpMcpPolicyResource{}
	_ resource.ResourceWithModifyPlan     = &dlpMcpPolicyResource{}
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
	Status       types.String      `tfsdk:"status"`
	Version      types.Int64       `tfsdk:"version"`
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
			"Baseline policy (`barndoor_dlp_mcp_baseline_policy`). Destroying a policy archives it: its " +
			"servers go back to the Baseline policy and its name is free to use again.\n\n" +
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
				MarkdownDescription: "IDs of the MCP servers the policy covers. An `ACTIVE` policy needs at " +
					"least one; set `status` to `DRAFT` or `INACTIVE` to save a policy with `[]`. An active " +
					"server can belong to only one policy. The `*` wildcard is not accepted: the Baseline policy covers " +
					"every server not listed in a policy.",
				ElementType: types.StringType,
				Required:    true,
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(
						noSurroundingWhitespace,
						stringvalidator.NoneOf("*"),
					),
				},
			},
			"rules": dlpMcpRulesAttribute("Rules applied to the policy's servers, in order. Use `[]` for a " +
				"policy with no rules, which exempts its servers from the Baseline policy. At most 50."),
			"status": schema.StringAttribute{
				MarkdownDescription: "The policy's lifecycle state: `" + policyStatusActive + "`, `" +
					policyStatusDraft + "` or `" + policyStatusInactive + "`. Only an `" + policyStatusActive +
					"` policy governs traffic. A `" + policyStatusDraft + "` or `" + policyStatusInactive +
					"` policy owns no servers (they follow another policy, or the Baseline policy) and may " +
					"list servers that other policies also list. Unlike `barndoor_policy`, a new policy is " +
					"created as `" + policyStatusActive + "` when `status` is not set. A new policy can only " +
					"be created as `" + policyStatusActive + "` or `" + policyStatusDraft + "`; to reach `" +
					policyStatusInactive + "` create it as `" + policyStatusActive + "` and change the " +
					"status in a later apply. `" + policyStatusArchived + "` is not configurable: archiving " +
					"is what `terraform destroy` does. When `status` is not set Terraform does not manage it, " +
					"so a status changed in the app is left alone. Setting `status` requires policy " +
					"lifecycle to be enabled for the organization; without it the API rejects any request " +
					"that carries a status, so leave it unset (a policy is then always `" +
					policyStatusActive + "`).",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					policyStatusValidator{},
				},
				// When status is not configured, carry the last-known value
				// through the plan instead of "(known after apply)", except
				// for state written before the attribute existed (null),
				// where the plan stays unknown so apply can fill it in.
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseNonNullStateForUnknown(),
				},
			},
			"version": schema.Int64Attribute{
				MarkdownDescription: "The policy's revision number; null until its first recorded change.",
				Computed:            true,
			},
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
	validateDlpMcpServersForStatus(ctx, req.Config, &resp.Diagnostics)
}

// validateDlpMcpServersForStatus requires at least one server unless the
// policy is configured as DRAFT or INACTIVE, which the API lets be saved with
// none. An unset status means ACTIVE for a new policy, so it needs servers
// too. Unknown values are skipped: they are checked again once known.
func validateDlpMcpServersForStatus(ctx context.Context, config tfsdk.Config, diags *diag.Diagnostics) {
	var servers types.Set
	var status types.String
	diags.Append(config.GetAttribute(ctx, path.Root("mcp_server_ids"), &servers)...)
	diags.Append(config.GetAttribute(ctx, path.Root("status"), &status)...)
	if diags.HasError() || servers.IsNull() || servers.IsUnknown() || status.IsUnknown() {
		return
	}
	if len(servers.Elements()) > 0 {
		return
	}
	if !status.IsNull() && (status.ValueString() == policyStatusDraft || status.ValueString() == policyStatusInactive) {
		return
	}
	diags.AddAttributeError(
		path.Root("mcp_server_ids"),
		"An ACTIVE policy needs at least one MCP server",
		"An ACTIVE policy needs at least one MCP server; set status to DRAFT or INACTIVE to save it without servers.",
	)
}

// ModifyPlan rejects creating a policy directly as INACTIVE: the API only
// accepts DRAFT or ACTIVE for a new policy. Transitions of an existing policy
// are left to the API, which names the rejected change in its 409.
func (r *dlpMcpPolicyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || !req.State.Raw.IsNull() {
		return
	}
	var status types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("status"), &status)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !status.IsNull() && !status.IsUnknown() && status.ValueString() == policyStatusInactive {
		resp.Diagnostics.AddAttributeError(
			path.Root("status"),
			"Cannot create a policy as INACTIVE",
			"A new policy can only be created as `"+policyStatusActive+"` or `"+policyStatusDraft+"`. Create it as `"+
				policyStatusActive+"` first, then set `status = \""+policyStatusInactive+"\"` in a later apply.",
		)
	}
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

	// Send status only when it is not the server default, so a configuration
	// that sets `status = "ACTIVE"` still works for an organization without
	// policy lifecycle (where any request carrying a status is refused).
	if !plan.Status.IsNull() && !plan.Status.IsUnknown() && plan.Status.ValueString() != policyStatusActive {
		status := plan.Status.ValueString()
		body.Status = &status
	}

	var policy dlpMcpPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPost, dlpAPIPrefix+"/mcp-policies", body, &policy); err != nil {
		addDlpMcpPolicyStatusAPIError(&resp.Diagnostics, "create the MCP policy", err)
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
	if plan.Status.IsUnknown() {
		warnDlpMcpPolicyInactive(&resp.Diagnostics, &policy)
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
	if policy.Status == dlpMcpStatusArchived {
		// Archived outside Terraform (an organization on policy lifecycle
		// reads it back instead of returning 404); drop it so Terraform
		// plans a recreate.
		resp.State.RemoveResource(ctx)
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
		if isNotFound(err) {
			addDlpMcpPolicyGoneDiagnostic(&resp.Diagnostics)
			return
		}
		addDlpMcpPolicyAPIError(&resp.Diagnostics, "read the MCP policy before updating it", err)
		return
	}
	if stored.Status == dlpMcpStatusArchived {
		addDlpMcpPolicyGoneDiagnostic(&resp.Diagnostics)
		return
	}
	var current dlpMcpPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &current)...)
	var configStatus types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("status"), &configStatus)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// A configured status is managed: move the policy to it when it differs
	// from the server's. Unset means Terraform leaves the status alone.
	desired := ""
	if !configStatus.IsNull() && !plan.Status.IsNull() && !plan.Status.IsUnknown() {
		desired = plan.Status.ValueString()
	}
	// The plan was made against the status held in state. If the server moved
	// since, only carry on when it already is where the plan wants it.
	if !current.Status.IsNull() && !current.Status.IsUnknown() && stored.Status != current.Status.ValueString() &&
		stored.Status != desired {
		resp.Diagnostics.AddError(
			"MCP policy status changed outside Terraform",
			fmt.Sprintf("The policy's status is now %s, but Terraform planned this change for status %s. Run "+
				"`terraform plan` again.", stored.Status, current.Status.ValueString()),
		)
		return
	}
	assignDlpMcpRuleIDs(body.Rules, stored.Rules)

	contentChanged, err := dlpMcpPolicyContentChanged(ctx, req)
	if err != nil {
		resp.Diagnostics.AddError("Failed to compare the MCP policy with its prior state", err.Error())
		return
	}
	statusChanged := desired != "" && desired != stored.Status

	policy := stored
	putContent := func() bool {
		if !contentChanged {
			return true
		}
		if err := doJSON(ctx, r.client, http.MethodPut, policyPath, body, &policy); err != nil {
			addDlpMcpPolicyAPIError(&resp.Diagnostics, "update the MCP policy", err)
			return false
		}
		return true
	}
	putStatus := func() bool {
		if !statusChanged {
			return true
		}
		// A status change travels alone: the API refuses it combined with
		// edits.
		verb := "change the MCP policy's status"
		if desired == policyStatusActive {
			verb = "activate the MCP policy"
		}
		if err := doJSON(ctx, r.client, http.MethodPut, policyPath, dlpMcpPolicyStatusRequest{Status: desired}, &policy); err != nil {
			addDlpMcpPolicyStatusAPIError(&resp.Diagnostics, verb, err)
			return false
		}
		return true
	}

	// Activating goes last, so its server-conflict check sees the new server
	// list. Moving off ACTIVE goes first, so the policy stops governing before
	// its content changes.
	steps := []func() bool{putContent, putStatus}
	if statusChanged && desired != policyStatusActive && stored.Status == policyStatusActive {
		steps = []func() bool{putStatus, putContent}
	}
	for _, step := range steps {
		if !step() {
			return
		}
	}

	newState, err := applyDlpMcpPolicyResponse(ctx, &policy, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	if desired == "" {
		warnDlpMcpPolicyInactive(&resp.Diagnostics, &policy)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// dlpMcpPolicyStatusRequest is the body of a status change, which the API
// accepts only on its own.
type dlpMcpPolicyStatusRequest struct {
	Status string `json:"status"`
}

// dlpMcpPolicyContentChanged reports whether the plan changes the policy's
// name, servers or rules relative to the prior state, so a change to status
// alone sends no content PUT.
func dlpMcpPolicyContentChanged(ctx context.Context, req resource.UpdateRequest) (bool, error) {
	var plannedName, priorName types.String
	var plannedServers, priorServers types.Set
	var plannedRules, priorRules types.List
	for _, g := range []struct {
		src interface {
			GetAttribute(context.Context, path.Path, interface{}) diag.Diagnostics
		}
		name   string
		target interface{}
	}{
		{req.Plan, "name", &plannedName}, {req.State, "name", &priorName},
		{req.Plan, "mcp_server_ids", &plannedServers}, {req.State, "mcp_server_ids", &priorServers},
		{req.Plan, "rules", &plannedRules}, {req.State, "rules", &priorRules},
	} {
		if diags := g.src.GetAttribute(ctx, path.Root(g.name), g.target); diags.HasError() {
			return false, fmt.Errorf("%s: %v", g.name, diags)
		}
	}
	return !plannedName.Equal(priorName) || !plannedServers.Equal(priorServers) || !plannedRules.Equal(priorRules), nil
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
	// Status is set on create only, and only when it is not the server
	// default; an update body never carries it (see dlpMcpPolicyStatusRequest).
	Status *string `json:"status,omitempty"`
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
		Status:       types.StringValue(policy.Status),
		Version:      types.Int64PointerValue(policy.Version),
		CreatedBy:    types.StringValue(policy.CreatedBy),
		UpdatedBy:    types.StringValue(policy.UpdatedBy),
		CreatedAt:    types.StringValue(policy.CreatedAt),
		UpdatedAt:    types.StringValue(policy.UpdatedAt),
	}, nil
}

// warnDlpMcpPolicyInactive warns about a policy that exists but, because it
// is not ACTIVE, does not govern traffic.
func warnDlpMcpPolicyInactive(diags *diag.Diagnostics, policy *dlpMcpPolicyResponse) {
	if policy.Status == "DRAFT" || policy.Status == "INACTIVE" {
		diags.AddWarning(
			"MCP policy does not govern traffic",
			fmt.Sprintf("MCP policy %q has status %s, so it does not currently govern traffic. This provider "+
				"version can't change a policy's status.", policy.Name, policy.Status),
		)
	}
}

// addDlpMcpPolicyGoneDiagnostic reports a policy that was archived or deleted
// between the last refresh and an update.
func addDlpMcpPolicyGoneDiagnostic(diags *diag.Diagnostics) {
	diags.AddError(
		"MCP policy no longer exists",
		"The policy was archived or deleted outside Terraform. Run `terraform plan` again to recreate it.",
	)
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
