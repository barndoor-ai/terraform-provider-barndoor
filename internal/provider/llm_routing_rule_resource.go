// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                     = &llmRoutingRuleResource{}
	_ resource.ResourceWithConfigure        = &llmRoutingRuleResource{}
	_ resource.ResourceWithImportState      = &llmRoutingRuleResource{}
	_ resource.ResourceWithConfigValidators = &llmRoutingRuleResource{}
)

// Routing-rule authoring bounds (`RoutingRuleDraft::validate`).
const (
	llmRoutingRuleMaxNameChars        = 120
	llmRoutingRuleMaxDescriptionChars = 2000
)

// NewLlmRoutingRuleResource returns a new barndoor_llm_routing_rule resource.
func NewLlmRoutingRuleResource() resource.Resource {
	return &llmRoutingRuleResource{}
}

// llmRoutingRuleResource manages a routing rule on a routing policy through
// the llm-gateway-service admin REST API (`/api/llm-gateway/admin/routing-rules`).
type llmRoutingRuleResource struct {
	client *client.Client
}

// llmRoutingRuleResourceModel maps the resource schema to Go types.
type llmRoutingRuleResourceModel struct {
	ID          types.String `tfsdk:"id"`
	OrgID       types.String `tfsdk:"org_id"`
	PolicyID    types.String `tfsdk:"policy_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	FloorSlot   types.Int64  `tfsdk:"floor_slot"`
	DenySlots   types.Set    `tfsdk:"deny_slots"`
	Enabled     types.Bool   `tfsdk:"enabled"`
}

func (r *llmRoutingRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_routing_rule"
}

func (r *llmRoutingRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a routing rule on a `barndoor_llm_routing_policy`: a plain-English " +
			"`description` of a kind of request, plus the constraint the gateway applies when the " +
			"policy's determiner decides a request matches it — a minimum slot (`floor_slot`), banned " +
			"slots (`deny_slots`), or both.\n\n" +
			"Slot numbers are indices into the policy's `slots` list (0 = first). The platform does not " +
			"check them against the policy: an index past the last slot is ignored at request time. " +
			"When rules contradict each other (for example one rule's floor is banned by another), the " +
			"platform still saves the rule and reports the conflict; the provider surfaces each " +
			"conflict that involves this rule as a warning.\n\n" +
			"Deleting the policy deletes its rules.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Routing rule UUID assigned by the API.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the rule belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"policy_id": schema.StringAttribute{
				MarkdownDescription: "UUID of the `barndoor_llm_routing_policy` the rule belongs to. " +
					"Changing it forces a new rule.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: fmt.Sprintf("Rule name, unique within the policy (case-sensitive; "+
					"at most %d characters).", llmRoutingRuleMaxNameChars),
				Required: true,
				Validators: []validator.String{
					noSurroundingWhitespace,
					stringvalidator.UTF8LengthAtMost(llmRoutingRuleMaxNameChars),
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: fmt.Sprintf("The kind of request the rule covers, in plain English "+
					"(at most %d characters). The determiner model reads it at request time to decide "+
					"whether a request matches, so write it as you would brief a person. Leading and "+
					"trailing whitespace is trimmed by the platform and ignored in diffs.",
					llmRoutingRuleMaxDescriptionChars),
				Required: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(regexp.MustCompile(`\S`), "must not be empty"),
					stringvalidator.UTF8LengthAtMost(llmRoutingRuleMaxDescriptionChars),
				},
			},
			"floor_slot": schema.Int64Attribute{
				MarkdownDescription: "Lowest slot index a matching request may use: requests the " +
					"determiner would send lower are raised to this slot.",
				Optional: true,
				Validators: []validator.Int64{
					int64validator.Between(0, 1<<31-1),
				},
			},
			"deny_slots": schema.SetAttribute{
				MarkdownDescription: "Slot indices a matching request must never use.",
				ElementType:         types.Int64Type,
				Optional:            true,
				Validators: []validator.Set{
					setvalidator.ValueInt64sAre(int64validator.Between(0, 1<<31-1)),
				},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the rule is applied. Defaults to `true`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
		},
	}
}

// ConfigValidators enforces the platform rule that a routing rule constrains
// something: a floor, at least one banned slot, or both.
func (r *llmRoutingRuleResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{llmRoutingRuleConstraintValidator{}}
}

// llmRoutingRuleConstraintValidator rejects a rule with neither floor_slot nor
// a non-empty deny_slots. resourcevalidator.AtLeastOneOf is not enough: an
// empty deny_slots set is "set" but constrains nothing.
type llmRoutingRuleConstraintValidator struct{}

func (v llmRoutingRuleConstraintValidator) Description(_ context.Context) string {
	return "at least one of floor_slot or a non-empty deny_slots must be set"
}

func (v llmRoutingRuleConstraintValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v llmRoutingRuleConstraintValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var floor types.Int64
	var deny types.Set
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("floor_slot"), &floor)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("deny_slots"), &deny)...)
	if resp.Diagnostics.HasError() || floor.IsUnknown() || deny.IsUnknown() {
		return
	}
	if floor.IsNull() && (deny.IsNull() || len(deny.Elements()) == 0) {
		resp.Diagnostics.AddAttributeError(path.Root("floor_slot"), "Routing rule constrains nothing",
			"Set floor_slot, a non-empty deny_slots, or both. A rule with neither would never "+
				"change any request, so the platform rejects it.")
	}
}

func (r *llmRoutingRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmRoutingRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmRoutingRuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := buildLlmRoutingRuleRequest(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	var rule llmRoutingRuleWriteResponse
	if err := doJSON(ctx, r.client, http.MethodPost,
		llmGatewayAPIPrefix+"/routing-rules?policy_id="+url.QueryEscape(plan.PolicyID.ValueString()),
		body, &rule); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM routing rule", "create the LLM routing rule", err)
		return
	}
	if rule.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no routing rule id.")
		return
	}

	addLlmRoutingRuleConflictWarnings(&resp.Diagnostics, rule.Name, rule.Conflicts)
	state, diags := applyLlmRoutingRuleResponse(&rule.llmRoutingRuleResponse, &plan)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Read refreshes the rule from its policy's rule listing: the admin API has
// no get-by-id endpoint for routing rules. A 404 means the policy itself is
// gone, which took the rule with it.
func (r *llmRoutingRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmRoutingRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var listing llmRoutingRuleListResponse
	err := doJSON(ctx, r.client, http.MethodGet,
		llmGatewayAPIPrefix+"/routing-rules?policy_id="+url.QueryEscape(state.PolicyID.ValueString()),
		nil, &listing)
	if isNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM routing rule", "list LLM routing rules", err)
		return
	}

	for i := range listing.Rules {
		if listing.Rules[i].ID == state.ID.ValueString() {
			newState, diags := applyLlmRoutingRuleResponse(&listing.Rules[i], &state)
			resp.Diagnostics.Append(diags...)
			resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
			return
		}
	}

	// Deleted out-of-band; drop the resource so Terraform plans a recreate.
	resp.State.RemoveResource(ctx)
}

// Update sends the whole rule: the platform's PUT is a full replace that
// fills anything omitted with the create defaults (no floor, no bans,
// enabled).
func (r *llmRoutingRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmRoutingRuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := buildLlmRoutingRuleRequest(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	var rule llmRoutingRuleWriteResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/routing-rules/"+plan.ID.ValueString(), body, &rule); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM routing rule", "update the LLM routing rule", err)
		return
	}

	addLlmRoutingRuleConflictWarnings(&resp.Diagnostics, rule.Name, rule.Conflicts)
	newState, diags := applyLlmRoutingRuleResponse(&rule.llmRoutingRuleResponse, &plan)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the rule. A 404 means it is already gone (possibly with its
// policy) — success for a destroy.
func (r *llmRoutingRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmRoutingRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/routing-rules/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM routing rule", "delete the LLM routing rule", err)
	}
}

// ImportState takes `<policy_id>/<rule_id>`: rules are only listable per
// policy, so a bare rule id cannot be located.
func (r *llmRoutingRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	policyID, ruleID, ok := strings.Cut(req.ID, "/")
	if !ok || policyID == "" || ruleID == "" || strings.Contains(ruleID, "/") {
		resp.Diagnostics.AddError("Invalid routing rule import ID",
			fmt.Sprintf("Expected an import ID of the form <policy_id>/<rule_id>, got %q. The LLM Gateway "+
				"API lists routing rules per policy and has no lookup by rule id alone, so the rule's "+
				"routing policy must be named too.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("policy_id"), policyID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), ruleID)...)
}

func (r *llmRoutingRuleResource) requireClient(diags *diag.Diagnostics) bool {
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

// llmRoutingRuleRequest mirrors the platform RoutingRuleDraft, the body of
// both create and the full-replace update. floor_slot is sent as an explicit
// null when unset so the replace semantics are visible on the wire.
type llmRoutingRuleRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	FloorSlot   *int32  `json:"floor_slot"`
	DenySlots   []int32 `json:"deny_slots"`
	Enabled     bool    `json:"enabled"`
}

// llmRoutingRuleResponse mirrors the platform RoutingRule.
type llmRoutingRuleResponse struct {
	ID          string  `json:"id"`
	OrgID       string  `json:"org_id"`
	PolicyID    string  `json:"policy_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	FloorSlot   *int32  `json:"floor_slot"`
	DenySlots   []int32 `json:"deny_slots"`
	Enabled     bool    `json:"enabled"`
}

// llmRoutingRuleConflict mirrors the platform RuleConflict: an advisory
// report that two enabled rules (or one rule with itself) cannot both hold.
type llmRoutingRuleConflict struct {
	Rule          string `json:"rule"`
	ConflictsWith string `json:"conflicts_with"`
	Detail        string `json:"detail"`
}

// llmRoutingRuleWriteResponse is the create/update response: the rule
// flattened, plus the policy's current conflicts.
type llmRoutingRuleWriteResponse struct {
	llmRoutingRuleResponse
	Conflicts []llmRoutingRuleConflict `json:"conflicts"`
}

// llmRoutingRuleListResponse is the listing response.
type llmRoutingRuleListResponse struct {
	Rules     []llmRoutingRuleResponse `json:"rules"`
	Conflicts []llmRoutingRuleConflict `json:"conflicts"`
}

// addLlmRoutingRuleConflictWarnings reports, as warnings, the conflicts that
// involve the rule just written. The platform returns every conflict on the
// policy; ones between two other rules were not caused by this write and
// would be noise here.
func addLlmRoutingRuleConflictWarnings(diags *diag.Diagnostics, name string, conflicts []llmRoutingRuleConflict) {
	for _, c := range conflicts {
		if c.Rule != name && c.ConflictsWith != name {
			continue
		}
		subject := fmt.Sprintf("%q conflicts with %q", c.Rule, c.ConflictsWith)
		if c.Rule == c.ConflictsWith {
			subject = fmt.Sprintf("%q contradicts itself", c.Rule)
		}
		diags.AddWarning("Routing rule conflict",
			fmt.Sprintf("The platform saved the rule, but reports that %s: %s", subject, c.Detail))
	}
}

// buildLlmRoutingRuleRequest converts the planned model to the full rule body.
func buildLlmRoutingRuleRequest(ctx context.Context, plan *llmRoutingRuleResourceModel) (*llmRoutingRuleRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	body := &llmRoutingRuleRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		FloorSlot:   int32PtrFromInt64(plan.FloorSlot),
		DenySlots:   []int32{},
		Enabled:     plan.Enabled.ValueBool(),
	}
	if !plan.DenySlots.IsNull() && !plan.DenySlots.IsUnknown() {
		var deny []int64
		diags.Append(plan.DenySlots.ElementsAs(ctx, &deny, false)...)
		for _, d := range deny {
			body.DenySlots = append(body.DenySlots, int32(d))
		}
	}
	return body, diags
}

// applyLlmRoutingRuleResponse maps the server's view onto a state model.
// prior is the plan (Create/Update) or previous state (Read/import). The
// platform trims the description, so a configured value that differs only
// in surrounding whitespace is kept; an empty deny_slots settles to null where
// prior had none.
func applyLlmRoutingRuleResponse(rule *llmRoutingRuleResponse, prior *llmRoutingRuleResourceModel) (llmRoutingRuleResourceModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	description := types.StringValue(rule.Description)
	if p, ok := knownString(prior.Description); ok && strings.TrimSpace(p) == rule.Description {
		description = prior.Description
	}

	deny := types.SetNull(types.Int64Type)
	if len(rule.DenySlots) > 0 || (!prior.DenySlots.IsNull() && !prior.DenySlots.IsUnknown()) {
		values := make([]attr.Value, 0, len(rule.DenySlots))
		for _, d := range rule.DenySlots {
			values = append(values, types.Int64Value(int64(d)))
		}
		var d diag.Diagnostics
		deny, d = types.SetValue(types.Int64Type, values)
		diags.Append(d...)
	}

	return llmRoutingRuleResourceModel{
		ID:          types.StringValue(rule.ID),
		OrgID:       types.StringValue(rule.OrgID),
		PolicyID:    types.StringValue(rule.PolicyID),
		Name:        types.StringValue(rule.Name),
		Description: description,
		FloorSlot:   int64FromInt32Ptr(rule.FloorSlot),
		DenySlots:   deny,
		Enabled:     types.BoolValue(rule.Enabled),
	}, diags
}
