// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
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
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	policyv2 "github.com/barndoor-ai/barndoor-go-sdk/proto/barndoor/public/policy/v2"
	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Practitioner-facing policy status strings. ARCHIVED is intentionally not
// configurable: archiving is the platform's terminal lifecycle state and is
// what `terraform destroy` performs.
const (
	policyStatusDraft    = "DRAFT"
	policyStatusActive   = "ACTIVE"
	policyStatusInactive = "INACTIVE"
	policyStatusArchived = "ARCHIVED"
)

// policyStatusToProto / policyStatusFromProto map the practitioner-facing
// status strings onto the proto enum. UNSPECIFIED never appears in state: the
// server resolves it to the DRAFT default on create.
var policyStatusToProto = map[string]policyv2.PolicyStatus{
	policyStatusDraft:    policyv2.PolicyStatus_POLICY_STATUS_DRAFT,
	policyStatusActive:   policyv2.PolicyStatus_POLICY_STATUS_ACTIVE,
	policyStatusInactive: policyv2.PolicyStatus_POLICY_STATUS_INACTIVE,
	policyStatusArchived: policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED,
}

var policyStatusFromProto = map[policyv2.PolicyStatus]string{
	policyv2.PolicyStatus_POLICY_STATUS_DRAFT:    policyStatusDraft,
	policyv2.PolicyStatus_POLICY_STATUS_ACTIVE:   policyStatusActive,
	policyv2.PolicyStatus_POLICY_STATUS_INACTIVE: policyStatusInactive,
	policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED: policyStatusArchived,
}

// effectToProto / effectFromProto map the rule effect strings onto the proto
// enum.
var effectToProto = map[string]policyv2.Effect{
	"ALLOW": policyv2.Effect_EFFECT_ALLOW,
	"DENY":  policyv2.Effect_EFFECT_DENY,
}

var effectFromProto = map[policyv2.Effect]string{
	policyv2.Effect_EFFECT_ALLOW: "ALLOW",
	policyv2.Effect_EFFECT_DENY:  "DENY",
}

// ruleNamePattern mirrors the API's rule-name restriction so a bad name fails
// at plan time instead of apply time.
var ruleNamePattern = regexp.MustCompile(`^[A-Za-z0-9 _-]+$`)

// policyUpdateMaskPaths is every field Update writes. The mask is ALWAYS sent
// with exactly these paths: Terraform's plan is the full desired state, and a
// masked empty field clears the server-side value — that is the point of the
// mask (an unmasked empty collection would be indistinguishable from "no
// change" under the API's legacy update semantics).
var policyUpdateMaskPaths = []string{
	"name",
	"description",
	"application_ids",
	"rules",
	"status",
	"tags",
	"support_contact",
}

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &policyResource{}
	_ resource.ResourceWithConfigure      = &policyResource{}
	_ resource.ResourceWithImportState    = &policyResource{}
	_ resource.ResourceWithModifyPlan     = &policyResource{}
	_ resource.ResourceWithValidateConfig = &policyResource{}
)

// NewPolicyResource returns a new barndoor_policy resource.
func NewPolicyResource() resource.Resource {
	return &policyResource{}
}

// policyResource manages an MCP-server access policy through the
// barndoor.policy.v2 gRPC contract.
type policyResource struct {
	client *client.Client
}

// policyResourceModel maps the resource schema to Go types. Rules is a plain
// slice so a null list (attribute omitted) round-trips as nil and an explicit
// empty list as a non-nil empty slice.
type policyResourceModel struct {
	ID             types.String      `tfsdk:"id"`
	Name           types.String      `tfsdk:"name"`
	McpServerID    types.String      `tfsdk:"mcp_server_id"`
	Description    types.String      `tfsdk:"description"`
	SupportContact types.String      `tfsdk:"support_contact"`
	Tags           types.Set         `tfsdk:"tags"`
	ApplicationIDs types.Set         `tfsdk:"application_ids"`
	Status         types.String      `tfsdk:"status"`
	Rules          []policyRuleModel `tfsdk:"rules"`
}

// policyRuleModel maps one entry of the rules list.
type policyRuleModel struct {
	Name        types.String         `tfsdk:"name"`
	Description types.String         `tfsdk:"description"`
	Effect      types.String         `tfsdk:"effect"`
	Active      types.Bool           `tfsdk:"active"`
	Actions     types.List           `tfsdk:"actions"`
	Roles       types.List           `tfsdk:"roles"`
	Condition   jsontypes.Normalized `tfsdk:"condition"`
}

func (r *policyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_policy"
}

func (r *policyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Barndoor access policy for an MCP server: which AI Agents it applies " +
			"to, its rules (effects, actions, roles, conditions), and its lifecycle status. " +
			"`terraform destroy` **archives** the policy — archival is the platform's terminal lifecycle " +
			"state; policies are never hard-deleted.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Policy UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Policy name. Must be unique among the organization's non-archived policies.",
				Required:            true,
			},
			"mcp_server_id": schema.StringAttribute{
				MarkdownDescription: "ID of the MCP server the policy governs. Immutable (BCP-2708): " +
					"changing it forces a new policy.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-form description of the policy.",
				Optional:            true,
			},
			"support_contact": schema.StringAttribute{
				MarkdownDescription: "Contact shown to end users when this policy blocks them (e.g. an email " +
					"address or support URL).",
				Optional: true,
			},
			"tags": schema.SetAttribute{
				MarkdownDescription: "Organizational tags attached to the policy.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"application_ids": schema.SetAttribute{
				MarkdownDescription: "IDs of the AI Agents (applications) this policy applies to.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "Lifecycle status: `" + policyStatusDraft + "` (the server default), `" +
					policyStatusActive + "`, or `" + policyStatusInactive + "`. A new policy can only be " +
					"created as `" + policyStatusDraft + "` or `" + policyStatusActive + "`; to reach `" +
					policyStatusInactive + "` create it as `" + policyStatusActive + "` and change the status " +
					"in a later apply. `" + policyStatusArchived + "` is not configurable — archiving is what " +
					"`terraform destroy` does.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					policyStatusValidator{},
				},
				// When status is not configured, carry the last-known value
				// through the plan instead of "(known after apply)": the
				// update path always masks `status`, and an unknown planned
				// status would have to be sent as an ambiguous empty value.
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"rules": schema.ListNestedAttribute{
				MarkdownDescription: "Ordered rules evaluated against tool calls. Omitting the attribute (or " +
					"setting `[]`) manages a policy with no rules; removing previously-configured rules " +
					"clears them on the server.",
				Optional:     true,
				NestedObject: policyRuleNestedObject(),
			},
		},
	}
}

// policyRuleNestedObject defines the schema for one rules entry.
func policyRuleNestedObject() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "Rule name. The API restricts it to letters, digits, spaces, hyphens, " +
					"and underscores.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(ruleNamePattern,
						"may only contain letters, digits, spaces, hyphens, and underscores"),
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-form description of the rule.",
				Optional:            true,
			},
			"effect": schema.StringAttribute{
				MarkdownDescription: "Whether matching calls are allowed or denied: `ALLOW` or `DENY`.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.OneOf("ALLOW", "DENY"),
				},
			},
			"active": schema.BoolAttribute{
				MarkdownDescription: "Whether the rule is enforced. Defaults to `true` on the server.",
				Optional:            true,
				Computed:            true,
				// Keep the last-known value through plans that change other
				// attributes; only genuinely new rules show "(known after
				// apply)" and take the server default.
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"actions": schema.ListAttribute{
				MarkdownDescription: "Tool/action names the rule matches. **Required** (with at least one " +
					"entry) because the API defaults an omitted list to `[\"*\"]` — everything — which is a " +
					"footgun when left implicit; say `[\"*\"]` explicitly to match all actions.",
				ElementType: types.StringType,
				Required:    true,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
				},
			},
			"roles": schema.ListAttribute{
				MarkdownDescription: "Principals the rule matches: `role:<name>`, `group:<name>`, or `*`. " +
					"**Required** (with at least one entry) for the same reason as `actions` — an omitted " +
					"list would silently default to `[\"*\"]`.",
				ElementType: types.StringType,
				Required:    true,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
				},
			},
			"condition": schema.StringAttribute{
				MarkdownDescription: "Optional condition tree as JSON (whitespace and key order are " +
					"insignificant). Each node is an object with exactly one of: `expr` " +
					"(`{\"expr\": \"<expression>\"}`) or `all`/`any`/`none` " +
					"(`{\"of\": [<condition>, ...]}`). Example: " +
					"`jsonencode({ all = { of = [{ expr = { expr = \"request.tool == 'search'\" } }] } })`.",
				Optional:   true,
				CustomType: jsontypes.NormalizedType{},
			},
		},
	}
}

// policyStatusValidator allows DRAFT/ACTIVE/INACTIVE and rejects everything
// else. ARCHIVED gets a dedicated message: asking for it in configuration is
// nonsensical — archiving is exactly what `terraform destroy` performs.
type policyStatusValidator struct{}

func (policyStatusValidator) Description(context.Context) string {
	return fmt.Sprintf("status must be one of %q, %q, %q", policyStatusDraft, policyStatusActive, policyStatusInactive)
}

func (v policyStatusValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v policyStatusValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	switch s := req.ConfigValue.ValueString(); s {
	case policyStatusDraft, policyStatusActive, policyStatusInactive:
	case policyStatusArchived:
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"ARCHIVED is not a configurable status",
			"Archiving is the policy's terminal lifecycle state and is what `terraform destroy` performs. "+
				"Remove the resource from configuration (or run a targeted destroy) instead of setting "+
				"`status = \""+policyStatusArchived+"\"`.",
		)
	default:
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid policy status",
			fmt.Sprintf("%s; got %q.", v.Description(context.Background()), s),
		)
	}
}

func (r *policyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		// Configure is called before the provider is configured (e.g. during
		// schema validation); nothing to wire up yet.
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

// ValidateConfig parses every known rule condition so structural mistakes
// (unknown keys, zero/multiple match operators) fail at plan time with the
// offending rule called out, instead of surfacing mid-apply. It walks the
// raw list value rather than the full model because config values may still
// be unknown at validation time.
func (r *policyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var rules types.List
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("rules"), &rules)...)
	if resp.Diagnostics.HasError() || rules.IsNull() || rules.IsUnknown() {
		return
	}

	for i, elem := range rules.Elements() {
		obj, ok := elem.(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() {
			continue
		}
		cond, ok := obj.Attributes()["condition"].(jsontypes.Normalized)
		if !ok || cond.IsNull() || cond.IsUnknown() {
			continue
		}
		if _, err := conditionFromJSON([]byte(cond.ValueString())); err != nil {
			resp.Diagnostics.AddAttributeError(
				path.Root("rules").AtListIndex(i).AtName("condition"),
				"Invalid rule condition",
				err.Error(),
			)
		}
	}
}

// ModifyPlan rejects creating a policy directly as INACTIVE: the API's
// lifecycle matrix only allows creation into DRAFT or ACTIVE.
func (r *policyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		// Destroy plan; nothing to validate.
		return
	}
	if !req.State.Raw.IsNull() {
		// Update: status transitions between DRAFT/ACTIVE/INACTIVE are the
		// API's to police.
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
			"The policy lifecycle only allows creating a policy as `"+policyStatusDraft+"` or `"+
				policyStatusActive+"`. Create it as `"+policyStatusActive+"` first, then set `status = \""+
				policyStatusInactive+"\"` in a later apply.",
		)
	}
}

func (r *policyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requirePolicyClient(&resp.Diagnostics) {
		return
	}

	var plan policyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	rpcReq, err := buildCreatePolicyRequest(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid policy configuration", err.Error())
		return
	}

	pc, err := r.policyClient(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the policy API", err.Error())
		return
	}

	rpcResp, err := pc.CreatePolicy(ctx, rpcReq)
	if err != nil {
		addPolicyRPCError(&resp.Diagnostics, "create the policy", err)
		return
	}
	detail := rpcResp.GetPolicy()
	if detail == nil {
		resp.Diagnostics.AddError("Malformed policy API response", "CreatePolicy returned no policy object.")
		return
	}

	state, err := applyPolicyDetail(ctx, detail, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the policy API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *policyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requirePolicyClient(&resp.Diagnostics) {
		return
	}

	var state policyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pc, err := r.policyClient(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the policy API", err.Error())
		return
	}

	rpcResp, err := pc.GetPolicy(ctx, &policyv2.GetPolicyRequest{PolicyId: state.ID.ValueString()})
	if err != nil {
		if grpcstatus.Code(err) == codes.NotFound {
			// The policy no longer exists; drop it so Terraform plans a recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addPolicyRPCError(&resp.Diagnostics, "read the policy", err)
		return
	}
	detail := rpcResp.GetPolicy()
	if detail == nil {
		resp.Diagnostics.AddError("Malformed policy API response", "GetPolicy returned no policy object.")
		return
	}

	// ARCHIVED is the platform's terminal state and is what this resource's
	// Delete performs, so in Terraform terms an archived policy is a deleted
	// one. Treat an out-of-band archive like a 404: remove the resource so the
	// next plan proposes a re-create (the archived row itself is unmanageable —
	// its name is freed for reuse and no transition leads back out).
	if detail.GetStatus() == policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED {
		resp.State.RemoveResource(ctx)
		return
	}

	newState, err := applyPolicyDetail(ctx, detail, &state)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the policy API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *policyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requirePolicyClient(&resp.Diagnostics) {
		return
	}

	var plan policyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	rpcReq, err := buildUpdatePolicyRequest(ctx, plan.ID.ValueString(), &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid policy configuration", err.Error())
		return
	}

	pc, err := r.policyClient(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the policy API", err.Error())
		return
	}

	rpcResp, err := pc.UpdatePolicy(ctx, rpcReq)
	if err != nil {
		addPolicyRPCError(&resp.Diagnostics, "update the policy", err)
		return
	}
	detail := rpcResp.GetPolicy()
	if detail == nil {
		resp.Diagnostics.AddError("Malformed policy API response", "UpdatePolicy returned no policy object.")
		return
	}

	newState, err := applyPolicyDetail(ctx, detail, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the policy API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete archives the policy (the platform's terminal state; there is no hard
// delete). A NotFound response means it is already gone, which is success for
// a destroy.
func (r *policyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requirePolicyClient(&resp.Diagnostics) {
		return
	}

	var state policyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pc, err := r.policyClient(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the policy API", err.Error())
		return
	}

	archived := policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED
	_, err = pc.UpdatePolicy(ctx, &policyv2.UpdatePolicyRequest{
		PolicyId:   state.ID.ValueString(),
		Status:     &archived,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status"}},
	})
	if err != nil && grpcstatus.Code(err) != codes.NotFound {
		addPolicyRPCError(&resp.Diagnostics, "archive (destroy) the policy", err)
	}
}

func (r *policyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// --- gRPC helpers --------------------------------------------------------------

func (r *policyResource) requirePolicyClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// policyClient builds a PolicyService client on the provider's shared lazy
// gRPC channel.
func (r *policyResource) policyClient(ctx context.Context) (policyv2.PolicyServiceClient, error) {
	conn, err := r.client.GRPCConn(ctx)
	if err != nil {
		return nil, err
	}
	return policyv2.NewPolicyServiceClient(conn), nil
}

// addPolicyRPCError turns a gRPC error into an actionable diagnostic. action
// is the failed verb phrase, e.g. "create the policy".
func addPolicyRPCError(diags *diag.Diagnostics, action string, err error) {
	st, ok := grpcstatus.FromError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch st.Code() {
	case codes.AlreadyExists:
		diags.AddError(
			"Policy name already in use",
			fmt.Sprintf("Failed to %s: a non-archived policy with this name already exists in the "+
				"organization. Policy names must be unique among non-archived policies; choose a different "+
				"`name` or archive the conflicting policy first.\n\nServer message: %s", action, st.Message()),
		)
	case codes.InvalidArgument:
		// The server's validation message is the most specific thing we can
		// show; surface it verbatim.
		diags.AddError("Policy rejected by the API", st.Message())
	case codes.PermissionDenied:
		diags.AddError(
			"Permission denied by the policy API",
			fmt.Sprintf("Failed to %s: the configured credential is not authorized. Confirm the "+
				"service-account credential carries the organization admin role and is scoped to the "+
				"organization that owns this policy.\n\nServer message: %s", action, st.Message()),
		)
	default:
		diags.AddError(
			"Failed to "+action,
			fmt.Sprintf("The policy API returned %s: %s", st.Code(), st.Message()),
		)
	}
}

// --- config/plan → RPC conversion ---------------------------------------------

// buildCreatePolicyRequest converts the planned model to a CreatePolicy RPC.
// An unset status stays POLICY_STATUS_UNSPECIFIED, which the server resolves
// to its DRAFT default.
func buildCreatePolicyRequest(ctx context.Context, plan *policyResourceModel) (*policyv2.CreatePolicyRequest, error) {
	req := &policyv2.CreatePolicyRequest{
		Name:        plan.Name.ValueString(),
		McpServerId: plan.McpServerID.ValueString(),
	}
	if v, ok := knownString(plan.Description); ok {
		req.Description = &v
	}
	if v, ok := knownString(plan.SupportContact); ok {
		req.SupportContact = &v
	}
	if v, ok := knownString(plan.Status); ok {
		req.Status = policyStatusToProto[v]
	}

	var err error
	if req.ApplicationIds, err = stringsFromSet(ctx, plan.ApplicationIDs); err != nil {
		return nil, fmt.Errorf("application_ids: %w", err)
	}
	if req.Tags, err = stringsFromSet(ctx, plan.Tags); err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	if req.Rules, err = buildPolicyRules(ctx, plan.Rules); err != nil {
		return nil, err
	}
	return req, nil
}

// buildUpdatePolicyRequest converts the planned model to an UpdatePolicy RPC
// carrying the full desired state under a mask of every managed path, so
// fields the practitioner removed from configuration are cleared server-side
// rather than silently left behind.
func buildUpdatePolicyRequest(ctx context.Context, id string, plan *policyResourceModel) (*policyv2.UpdatePolicyRequest, error) {
	name := plan.Name.ValueString()
	req := &policyv2.UpdatePolicyRequest{
		PolicyId:   id,
		Name:       &name,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: policyUpdateMaskPaths},
	}
	if v, ok := knownString(plan.Description); ok {
		req.Description = &v
	}
	if v, ok := knownString(plan.SupportContact); ok {
		req.SupportContact = &v
	}
	if v, ok := knownString(plan.Status); ok {
		st, exists := policyStatusToProto[v]
		if !exists {
			return nil, fmt.Errorf("unsupported status %q", v)
		}
		req.Status = &st
	}

	var err error
	if req.ApplicationIds, err = stringsFromSet(ctx, plan.ApplicationIDs); err != nil {
		return nil, fmt.Errorf("application_ids: %w", err)
	}
	if req.Tags, err = stringsFromSet(ctx, plan.Tags); err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	if req.Rules, err = buildPolicyRules(ctx, plan.Rules); err != nil {
		return nil, err
	}
	return req, nil
}

// buildPolicyRules converts the planned rules to proto. A nil slice (rules
// omitted) converts to nil, which under the always-sent update mask clears
// the server-side rules.
func buildPolicyRules(ctx context.Context, rules []policyRuleModel) ([]*policyv2.PolicyRule, error) {
	if rules == nil {
		return nil, nil
	}
	out := make([]*policyv2.PolicyRule, 0, len(rules))
	for i, rm := range rules {
		pr := &policyv2.PolicyRule{}
		if v, ok := knownString(rm.Name); ok {
			pr.Name = &v
		}
		if v, ok := knownString(rm.Description); ok {
			pr.Description = &v
		}
		effect, ok := effectToProto[rm.Effect.ValueString()]
		if !ok {
			return nil, fmt.Errorf("rules[%d]: unsupported effect %q", i, rm.Effect.ValueString())
		}
		pr.Effect = effect
		// active is optional+computed: only a value the practitioner set (or
		// prior state carried) is sent; otherwise the server default applies.
		if !rm.Active.IsNull() && !rm.Active.IsUnknown() {
			v := rm.Active.ValueBool()
			pr.Active = &v
		}

		var err error
		if pr.Actions, err = stringsFromList(ctx, rm.Actions); err != nil {
			return nil, fmt.Errorf("rules[%d].actions: %w", i, err)
		}
		if pr.Roles, err = stringsFromList(ctx, rm.Roles); err != nil {
			return nil, fmt.Errorf("rules[%d].roles: %w", i, err)
		}
		if !rm.Condition.IsNull() && !rm.Condition.IsUnknown() {
			cond, err := conditionFromJSON([]byte(rm.Condition.ValueString()))
			if err != nil {
				return nil, fmt.Errorf("rules[%d].condition: %w", i, err)
			}
			pr.Condition = cond
		}
		out = append(out, pr)
	}
	return out, nil
}

// --- RPC → state conversion -----------------------------------------------------

// applyPolicyDetail maps the server's PolicyDetail onto a state model. prior
// is the plan (Create/Update) or the previous state (Read); it disambiguates
// the null-vs-empty cases the wire format collapses, so a value the
// practitioner never set does not come back as a spurious diff.
func applyPolicyDetail(ctx context.Context, detail *policyv2.PolicyDetail, prior *policyResourceModel) (policyResourceModel, error) {
	statusStr, ok := policyStatusFromProto[detail.GetStatus()]
	if !ok {
		return policyResourceModel{}, fmt.Errorf("policy API returned unknown status %q", detail.GetStatus())
	}

	m := policyResourceModel{
		ID:             types.StringValue(detail.GetId()),
		Name:           types.StringValue(detail.GetName()),
		McpServerID:    types.StringValue(detail.GetMcpServerId()),
		Status:         types.StringValue(statusStr),
		Description:    optionalStringFromPtr(detail.Description, prior.Description),
		SupportContact: optionalStringFromPtr(detail.SupportContact, prior.SupportContact),
	}

	var err error
	if m.Tags, err = setFromStrings(ctx, detail.GetTags(), prior.Tags); err != nil {
		return policyResourceModel{}, fmt.Errorf("tags: %w", err)
	}
	if m.ApplicationIDs, err = setFromStrings(ctx, detail.GetApplicationIds(), prior.ApplicationIDs); err != nil {
		return policyResourceModel{}, fmt.Errorf("application_ids: %w", err)
	}
	if m.Rules, err = rulesFromProto(ctx, detail.GetRules(), prior.Rules); err != nil {
		return policyResourceModel{}, err
	}
	return m, nil
}

// rulesFromProto maps the server's rules to model entries. Zero server rules
// settle to nil (a null list) when the prior model had none configured, and
// to an explicit empty slice when it carried an empty list.
func rulesFromProto(ctx context.Context, protoRules []*policyv2.PolicyRule, prior []policyRuleModel) ([]policyRuleModel, error) {
	if len(protoRules) == 0 {
		if prior == nil {
			return nil, nil
		}
		return []policyRuleModel{}, nil
	}

	out := make([]policyRuleModel, 0, len(protoRules))
	for i, pr := range protoRules {
		effectStr, ok := effectFromProto[pr.GetEffect()]
		if !ok {
			return nil, fmt.Errorf("rules[%d]: policy API returned unknown effect %q", i, pr.GetEffect())
		}

		// prior rules pair with server rules by index (the server preserves
		// rule order).
		var priorName, priorDescription types.String
		if i < len(prior) {
			priorName = prior[i].Name
			priorDescription = prior[i].Description
		}

		rm := policyRuleModel{
			Name:        optionalStringFromPtr(pr.Name, priorName),
			Description: optionalStringFromPtr(pr.Description, priorDescription),
			Effect:      types.StringValue(effectStr),
			// The server always populates active on read; tolerate an absent
			// value by settling on the documented server default.
			Active: types.BoolValue(true),
		}
		if pr.Active != nil {
			rm.Active = types.BoolValue(*pr.Active)
		}

		var diags diag.Diagnostics
		if rm.Actions, diags = types.ListValueFrom(ctx, types.StringType, pr.GetActions()); diags.HasError() {
			return nil, fmt.Errorf("rules[%d].actions: %v", i, diags)
		}
		if rm.Roles, diags = types.ListValueFrom(ctx, types.StringType, pr.GetRoles()); diags.HasError() {
			return nil, fmt.Errorf("rules[%d].roles: %v", i, diags)
		}

		rm.Condition = jsontypes.NewNormalizedNull()
		if pr.Condition != nil {
			s, err := conditionToJSON(pr.Condition)
			if err != nil {
				return nil, fmt.Errorf("rules[%d].condition: %w", i, err)
			}
			rm.Condition = jsontypes.NewNormalizedValue(s)
		}
		out = append(out, rm)
	}
	return out, nil
}

// optionalStringFromPtr maps an optional wire string to state: nil means
// absent (null), and an empty string settles back to null when the prior
// value was null/unknown — the server cannot distinguish "cleared" from
// "never set", and echoing "" where the config said nothing would be
// perpetual drift.
func optionalStringFromPtr(p *string, prior types.String) types.String {
	if p == nil {
		return types.StringNull()
	}
	if *p == "" && (prior.IsNull() || prior.IsUnknown()) {
		return types.StringNull()
	}
	return types.StringValue(*p)
}

// setFromStrings maps a wire string collection to a set, settling an empty
// collection to null when the prior value was null/unknown (same
// null-vs-empty reasoning as optionalStringFromPtr).
func setFromStrings(ctx context.Context, vals []string, prior types.Set) (types.Set, error) {
	if len(vals) == 0 && (prior.IsNull() || prior.IsUnknown()) {
		return types.SetNull(types.StringType), nil
	}
	if vals == nil {
		vals = []string{}
	}
	set, diags := types.SetValueFrom(ctx, types.StringType, vals)
	if diags.HasError() {
		return types.SetNull(types.StringType), fmt.Errorf("%v", diags)
	}
	return set, nil
}

// --- condition JSON ⇄ proto -----------------------------------------------------

// conditionMatchKeys is the exhaustive set of operator keys a condition node
// may carry (exactly one of them).
var conditionMatchKeys = []string{"expr", "all", "any", "none"}

// conditionFromJSON parses a practitioner condition tree (JSON) into the
// proto condition. Parsing is strict: every node must be an object with
// exactly one of the match keys, and no unknown fields are tolerated —
// anything else is reported with the path to the offending node.
func conditionFromJSON(raw []byte) (*policyv2.PolicyRuleCondition, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("condition must be a JSON object: %w", err)
	}
	if len(obj) != 1 {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return nil, fmt.Errorf(
			"a condition object must have exactly one of %q; got %d keys [%s]",
			conditionMatchKeys, len(obj), strings.Join(keys, ", "))
	}

	for key, val := range obj {
		switch key {
		case "expr":
			var e struct {
				Expr *string `json:"expr"`
			}
			if err := strictUnmarshalJSON(val, &e); err != nil {
				return nil, fmt.Errorf(`"expr" must be an object of the form {"expr": "<expression>"}: %w`, err)
			}
			if e.Expr == nil {
				return nil, fmt.Errorf(`"expr" object is missing its "expr" string`)
			}
			return &policyv2.PolicyRuleCondition{
				Match: &policyv2.PolicyRuleCondition_Expr{Expr: &policyv2.Expression{Expr: *e.Expr}},
			}, nil

		case "all", "any", "none":
			var op struct {
				Of []json.RawMessage `json:"of"`
			}
			if err := strictUnmarshalJSON(val, &op); err != nil {
				return nil, fmt.Errorf(`%q must be an object of the form {"of": [<condition>, ...]}: %w`, key, err)
			}
			if op.Of == nil {
				return nil, fmt.Errorf(`%q object is missing its "of" array`, key)
			}
			children := make([]*policyv2.PolicyRuleCondition, 0, len(op.Of))
			for i, c := range op.Of {
				child, err := conditionFromJSON(c)
				if err != nil {
					return nil, fmt.Errorf("%s.of[%d]: %w", key, i, err)
				}
				children = append(children, child)
			}
			cond := &policyv2.PolicyRuleCondition{}
			switch key {
			case "all":
				cond.Match = &policyv2.PolicyRuleCondition_All{All: &policyv2.OperatorAll{Of: children}}
			case "any":
				cond.Match = &policyv2.PolicyRuleCondition_Any{Any: &policyv2.OperatorAny{Of: children}}
			case "none":
				cond.Match = &policyv2.PolicyRuleCondition_None{None: &policyv2.OperatorNone{Of: children}}
			}
			return cond, nil

		default:
			return nil, fmt.Errorf("unknown condition key %q; expected one of %q", key, conditionMatchKeys)
		}
	}
	// Unreachable: len(obj) == 1 guarantees the loop returns.
	return nil, fmt.Errorf("empty condition object")
}

// strictUnmarshalJSON decodes raw into v, rejecting unknown fields.
func strictUnmarshalJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// conditionToJSON renders the proto condition as canonical JSON (sorted keys,
// no insignificant whitespace) for state. jsontypes.Normalized compares
// semantically, so a differently-formatted config never diffs against it.
func conditionToJSON(cond *policyv2.PolicyRuleCondition) (string, error) {
	v, err := conditionToValue(cond)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode condition: %w", err)
	}
	return string(b), nil
}

// conditionToValue converts one proto condition node into the JSON object
// shape documented on the schema.
func conditionToValue(cond *policyv2.PolicyRuleCondition) (any, error) {
	switch m := cond.GetMatch().(type) {
	case *policyv2.PolicyRuleCondition_Expr:
		return map[string]any{"expr": map[string]any{"expr": m.Expr.GetExpr()}}, nil
	case *policyv2.PolicyRuleCondition_All:
		return operatorToValue("all", m.All.GetOf())
	case *policyv2.PolicyRuleCondition_Any:
		return operatorToValue("any", m.Any.GetOf())
	case *policyv2.PolicyRuleCondition_None:
		return operatorToValue("none", m.None.GetOf())
	default:
		return nil, fmt.Errorf("policy API returned a condition with no match operator set")
	}
}

func operatorToValue(key string, of []*policyv2.PolicyRuleCondition) (map[string]any, error) {
	children := make([]any, 0, len(of))
	for i, c := range of {
		v, err := conditionToValue(c)
		if err != nil {
			return nil, fmt.Errorf("%s.of[%d]: %w", key, i, err)
		}
		children = append(children, v)
	}
	return map[string]any{key: map[string]any{"of": children}}, nil
}

// --- small value helpers ---------------------------------------------------------

// knownString returns the value and true when v is a known, non-null string.
func knownString(v types.String) (string, bool) {
	if v.IsNull() || v.IsUnknown() {
		return "", false
	}
	return v.ValueString(), true
}

// stringsFromList converts a list of strings to a Go slice; null/unknown
// convert to nil.
func stringsFromList(ctx context.Context, l types.List) ([]string, error) {
	if l.IsNull() || l.IsUnknown() {
		return nil, nil
	}
	out := []string{}
	if diags := l.ElementsAs(ctx, &out, false); diags.HasError() {
		return nil, fmt.Errorf("%v", diags)
	}
	return out, nil
}

// stringsFromSet converts a set of strings to a Go slice; null/unknown
// convert to nil.
func stringsFromSet(ctx context.Context, s types.Set) ([]string, error) {
	if s.IsNull() || s.IsUnknown() {
		return nil, nil
	}
	out := []string{}
	if diags := s.ElementsAs(ctx, &out, false); diags.HasError() {
		return nil, fmt.Errorf("%v", diags)
	}
	return out, nil
}
