// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/http"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmBudgetPeriods / llmBudgetActions are the BudgetPeriod / BudgetAction
// enums' wire values.
var (
	llmBudgetPeriods = []string{"daily", "weekly", "monthly"}
	llmBudgetActions = []string{"block", "throttle", "warn"}
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                     = &llmTokenBudgetResource{}
	_ resource.ResourceWithConfigure        = &llmTokenBudgetResource{}
	_ resource.ResourceWithImportState      = &llmTokenBudgetResource{}
	_ resource.ResourceWithConfigValidators = &llmTokenBudgetResource{}
)

// NewLlmTokenBudgetResource returns a new barndoor_llm_token_budget resource.
func NewLlmTokenBudgetResource() resource.Resource {
	return &llmTokenBudgetResource{}
}

// llmTokenBudgetResource manages an LLM Gateway token budget through the
// llm-gateway-service admin REST API (`/api/llm-gateway/admin/budgets`).
type llmTokenBudgetResource struct {
	client *client.Client
}

// llmTokenBudgetResourceModel maps the resource schema to Go types.
type llmTokenBudgetResourceModel struct {
	ID              types.String  `tfsdk:"id"`
	OrgID           types.String  `tfsdk:"org_id"`
	Name            types.String  `tfsdk:"name"`
	ScopeType       types.String  `tfsdk:"scope_type"`
	ScopeID         types.String  `tfsdk:"scope_id"`
	ScopeValue      types.String  `tfsdk:"scope_value"`
	MemberOfGroup   types.String  `tfsdk:"member_of_group"`
	Period          types.String  `tfsdk:"period"`
	TokenLimit      types.Int64   `tfsdk:"token_limit"`
	CostLimit       types.Float64 `tfsdk:"cost_limit"`
	Currency        types.String  `tfsdk:"currency"`
	AlertThresholds types.List    `tfsdk:"alert_thresholds"`
	ActionOnExhaust types.String  `tfsdk:"action_on_exhaust"`
	TrafficType     types.String  `tfsdk:"traffic_type"`
	Enabled         types.Bool    `tfsdk:"enabled"`
	ChangeNote      types.String  `tfsdk:"change_note"`
	llmTargetsModel
	CreatedAt types.String `tfsdk:"created_at"`
}

// llmTargetsModel holds the target dimensions shared by the token-budget and
// rate-limit resources (see llmTargetAttributes).
type llmTargetsModel struct {
	TargetProviderID    types.String `tfsdk:"target_provider_id"`
	TargetUpstreamModel types.String `tfsdk:"target_upstream_model"`
	TargetModelAlias    types.String `tfsdk:"target_model_alias"`
	TargetMcpServerID   types.String `tfsdk:"target_mcp_server_id"`
}

func (r *llmTokenBudgetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_token_budget"
}

func (r *llmTokenBudgetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attributes := map[string]schema.Attribute{
		"id": schema.StringAttribute{
			MarkdownDescription: "Budget UUID assigned by the API; also the `terraform import` key.",
			Computed:            true,
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		},
		"org_id": schema.StringAttribute{
			MarkdownDescription: "Organization the budget belongs to, resolved from the provider " +
				"credential's token claims.",
			Computed: true,
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		},
		"name": schema.StringAttribute{
			MarkdownDescription: "Human-readable budget name.",
			Required:            true,
		},
		"scope_type": llmScopeTypeAttribute(llmGatewayIdentityScopeTypes, true),
		"scope_id": schema.StringAttribute{
			MarkdownDescription: "UUID of the scoped entity (user, team, API key, …) for UUID-keyed " +
				"scope types. Changing it forces a new budget (the API has no update for it).",
			Optional: true,
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		},
		"scope_value": schema.StringAttribute{
			MarkdownDescription: "String key of the scoped entity for name-keyed scope types (e.g. a " +
				"role or IdP group name for `role`/`group` scopes). Changing it forces a new budget " +
				"(the API has no update for it).",
			Optional: true,
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		},
		"member_of_group": llmMemberOfGroupAttribute("budget"),
		"period": schema.StringAttribute{
			MarkdownDescription: "Budget window: `daily`, `weekly` (Monday-anchored), or `monthly`.",
			Required:            true,
			Validators: []validator.String{
				stringvalidator.OneOf(llmBudgetPeriods...),
			},
		},
		"token_limit": schema.Int64Attribute{
			MarkdownDescription: "Tokens allowed per period (at least 1). Omit it, and set " +
				"`cost_limit`, for a cost-only budget. At least one of the two is required.",
			Optional: true,
			Validators: []validator.Int64{
				int64validator.AtLeast(1),
			},
		},
		"cost_limit": schema.Float64Attribute{
			MarkdownDescription: "Spend allowed per period, in `currency`, computed from the " +
				"organization's model pricing. Greater than 0, at most 99,999,999.9999, with at most " +
				"four decimal places (the platform stores `DECIMAL(12, 4)`). With `token_limit` too, " +
				"whichever is reached first exhausts the budget. Removing it clears the cost limit in " +
				"place.",
			Optional: true,
			Validators: []validator.Float64{
				llmCostLimitValidator{},
			},
		},
		"currency": schema.StringAttribute{
			MarkdownDescription: "ISO 4217 code `cost_limit` is expressed in. Defaults to `USD`. " +
				"Changing it forces a new budget (the API cannot update it).",
			Optional: true,
			Computed: true,
			Default:  stringdefault.StaticString("USD"),
			Validators: []validator.String{
				stringvalidator.RegexMatches(regexp.MustCompile(`^[A-Z]{3}$`),
					"must be a three-letter upper-case ISO 4217 code"),
			},
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		},
		"alert_thresholds": schema.ListAttribute{
			MarkdownDescription: "Usage percentages (0–100) at which the platform raises alerts. " +
				"Defaults to `[80, 90]`.",
			ElementType: types.Int64Type,
			Optional:    true,
			Computed:    true,
			Validators: []validator.List{
				listvalidator.ValueInt64sAre(int64validator.Between(0, 100)),
			},
			PlanModifiers: []planmodifier.List{
				listplanmodifier.UseStateForUnknown(),
			},
		},
		"action_on_exhaust": schema.StringAttribute{
			MarkdownDescription: "What happens when the budget is exhausted: `block`, `throttle`, or " +
				"`warn`. Defaults to `block`.",
			Optional: true,
			Computed: true,
			Default:  stringdefault.StaticString("block"),
			Validators: []validator.String{
				stringvalidator.OneOf(llmBudgetActions...),
			},
		},
		"traffic_type": llmTrafficTypeAttribute("all", stringdefault.StaticString("all")),
		"enabled":      llmEnabledAttribute(),
		"change_note":  llmChangeNoteAttribute("budget"),
		"created_at": schema.StringAttribute{
			MarkdownDescription: "When the budget was created (RFC 3339).",
			Computed:            true,
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		},
	}
	maps.Copy(attributes, llmTargetAttributes("budget", true))

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway budget: a periodic token and/or spend ceiling " +
			"applied to an identity scope (the organization, a user, an IdP group, …), optionally " +
			"narrowed to one provider, upstream model, model alias or MCP server, with alerting " +
			"thresholds and an exhaustion action.\n\n" +
			"The scope, `member_of_group`, `currency` and target attributes are immutable — the update " +
			"API cannot change them, so changing any of them forces a new budget. The platform allows " +
			"one budget per `(scope, member_of_group, targets, traffic_type, period)` combination and " +
			"answers 409 on duplicates.",
		Attributes: attributes,
	}
}

// llmCostLimitValidator checks cost_limit against the DECIMAL(12, 4) column:
// a value the column would round reads back different from the configuration,
// which is a permanent diff rather than an error.
type llmCostLimitValidator struct{}

func (v llmCostLimitValidator) Description(_ context.Context) string {
	return "must be greater than 0 and at most 99999999.9999, with at most four decimal places"
}

func (v llmCostLimitValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v llmCostLimitValidator) ValidateFloat64(_ context.Context, req validator.Float64Request, resp *validator.Float64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	f := req.ConfigValue.ValueFloat64()
	scaled := f * 10000
	if f <= 0 || f > 99999999.9999 || math.Abs(scaled-math.Round(scaled)) > 1e-6 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid cost_limit",
			fmt.Sprintf("cost_limit %v %s.", f, v.Description(context.Background())))
	}
}

// ConfigValidators enforces the platform's at-least-one-limit, member_of_group
// and target shape rules at plan time.
func (r *llmTokenBudgetResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.AtLeastOneOf(
			path.MatchRoot("token_limit"),
			path.MatchRoot("cost_limit"),
		),
		llmMemberOfGroupShapeValidator{what: "budget"},
		llmTargetShapeValidator{},
	}
}

func (r *llmTokenBudgetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmTokenBudgetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmTokenBudgetResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// token_limit is a required key on create: a cost-only budget sends 0.
	body := &llmTokenBudgetCreateRequest{
		Name:           plan.Name.ValueString(),
		ScopeType:      plan.ScopeType.ValueString(),
		Period:         plan.Period.ValueString(),
		TokenLimit:     plan.TokenLimit.ValueInt64(),
		CostLimit:      float64PtrFromFloat64(plan.CostLimit),
		llmTargetsWire: plan.wire(),
	}
	if v, ok := knownString(plan.Currency); ok {
		body.Currency = &v
	}
	if v, ok := knownString(plan.ChangeNote); ok {
		body.ChangeNote = &v
	}
	if v, ok := knownString(plan.ScopeID); ok {
		body.ScopeID = &v
	}
	if v, ok := knownString(plan.ScopeValue); ok {
		body.ScopeValue = &v
	}
	if v, ok := knownString(plan.MemberOfGroup); ok {
		body.MemberOfGroup = &v
	}
	if v, ok := knownString(plan.ActionOnExhaust); ok {
		body.ActionOnExhaust = &v
	}
	if v, ok := knownString(plan.TrafficType); ok {
		body.TrafficType = &v
	}
	thresholds, err := int64sFromList(ctx, plan.AlertThresholds)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("alert_thresholds"), "Invalid alert_thresholds", err.Error())
		return
	}
	body.AlertThresholds = thresholds

	var budget llmTokenBudgetResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/budgets", body, &budget); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM token budget", "create the LLM token budget", err)
		return
	}
	if budget.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no budget id.")
		return
	}

	// Record the created budget before any follow-up call so a failure below
	// still leaves it tracked rather than orphaned.
	state, diags := applyLlmTokenBudgetResponse(ctx, &budget, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The create endpoint has no `enabled` field (new budgets are always
	// enabled); converge an `enabled = false` plan with an immediate update.
	if enabled, ok := knownBool(plan.Enabled); ok && !enabled {
		disable := false
		if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/budgets/"+budget.ID,
			&llmTokenBudgetUpdateRequest{Enabled: &disable}, &budget); err != nil {
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM token budget",
				"disable the LLM token budget after create", err)
			return
		}
		state, diags = applyLlmTokenBudgetResponse(ctx, &budget, &plan)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	}
}

// Read refreshes the budget from the org-wide listing: the admin API has no
// get-by-id endpoint for budgets.
func (r *llmTokenBudgetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmTokenBudgetResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var budgets []llmTokenBudgetResponse
	if err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/budgets", nil, &budgets); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM token budget", "list LLM token budgets", err)
		return
	}

	for i := range budgets {
		if budgets[i].ID == state.ID.ValueString() {
			newState, diags := applyLlmTokenBudgetResponse(ctx, &budgets[i], &state)
			resp.Diagnostics.Append(diags...)
			if resp.Diagnostics.HasError() {
				return
			}
			resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
			return
		}
	}

	// Deleted out-of-band; drop the resource so Terraform plans a recreate.
	resp.State.RemoveResource(ctx)
}

func (r *llmTokenBudgetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmTokenBudgetResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The update COALESCEs every column, so a removed limit is cleared with a
	// zero: token_limit 0 is the cost-only shape, and cost_limit 0 is the
	// API's spelling of "no cost limit" (JSON null would keep it).
	name := plan.Name.ValueString()
	tokenLimit := plan.TokenLimit.ValueInt64()
	costLimit := 0.0
	if v := float64PtrFromFloat64(plan.CostLimit); v != nil {
		costLimit = *v
	}
	period := plan.Period.ValueString()
	body := &llmTokenBudgetUpdateRequest{
		Name:       &name,
		TokenLimit: &tokenLimit,
		CostLimit:  &costLimit,
		Period:     &period,
		Enabled:    boolPtrFromBool(plan.Enabled),
	}
	if v, ok := knownString(plan.ChangeNote); ok {
		body.ChangeNote = &v
	}
	if v, ok := knownString(plan.ActionOnExhaust); ok {
		body.ActionOnExhaust = &v
	}
	if v, ok := knownString(plan.TrafficType); ok {
		body.TrafficType = &v
	}
	thresholds, err := int64sFromList(ctx, plan.AlertThresholds)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("alert_thresholds"), "Invalid alert_thresholds", err.Error())
		return
	}
	body.AlertThresholds = thresholds

	var budget llmTokenBudgetResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/budgets/"+plan.ID.ValueString(), body, &budget); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM token budget", "update the LLM token budget", err)
		return
	}

	newState, diags := applyLlmTokenBudgetResponse(ctx, &budget, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the budget. A 404 means it is already gone — success for a
// destroy.
func (r *llmTokenBudgetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmTokenBudgetResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/budgets/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM token budget", "delete the LLM token budget", err)
	}
}

func (r *llmTokenBudgetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmTokenBudgetResource) requireClient(diags *diag.Diagnostics) bool {
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

// llmTokenBudgetCreateRequest mirrors the llm-gateway CreateBudgetRequest body.
type llmTokenBudgetCreateRequest struct {
	Name            string   `json:"name"`
	ScopeType       string   `json:"scope_type"`
	ScopeID         *string  `json:"scope_id,omitempty"`
	ScopeValue      *string  `json:"scope_value,omitempty"`
	MemberOfGroup   *string  `json:"member_of_group,omitempty"`
	Period          string   `json:"period"`
	TokenLimit      int64    `json:"token_limit"`
	CostLimit       *float64 `json:"cost_limit,omitempty"`
	Currency        *string  `json:"currency,omitempty"`
	AlertThresholds []int64  `json:"alert_thresholds,omitempty"`
	ActionOnExhaust *string  `json:"action_on_exhaust,omitempty"`
	TrafficType     *string  `json:"traffic_type,omitempty"`
	ChangeNote      *string  `json:"change_note,omitempty"`
	llmTargetsWire
}

// llmTargetsWire is the target dimensions' wire shape. The keys are not
// omitempty: the rate-limit update clears a target with an explicit null,
// and on create a null is the same as an absent key.
type llmTargetsWire struct {
	TargetProviderID    *string `json:"target_provider_id"`
	TargetUpstreamModel *string `json:"target_upstream_model"`
	TargetModelAlias    *string `json:"target_model_alias"`
	TargetMcpServerID   *string `json:"target_mcp_server_id"`
}

// wire converts the planned targets; null/unknown become null.
func (m llmTargetsModel) wire() llmTargetsWire {
	ptr := func(v types.String) *string {
		if s, ok := knownString(v); ok {
			return &s
		}
		return nil
	}
	return llmTargetsWire{
		TargetProviderID:    ptr(m.TargetProviderID),
		TargetUpstreamModel: ptr(m.TargetUpstreamModel),
		TargetModelAlias:    ptr(m.TargetModelAlias),
		TargetMcpServerID:   ptr(m.TargetMcpServerID),
	}
}

// model maps the server's targets onto state, settling "" to null like every
// other optional string.
func (w llmTargetsWire) model(prior llmTargetsModel) llmTargetsModel {
	return llmTargetsModel{
		TargetProviderID:    optionalStringFromPtr(w.TargetProviderID, prior.TargetProviderID),
		TargetUpstreamModel: optionalStringFromPtr(w.TargetUpstreamModel, prior.TargetUpstreamModel),
		TargetModelAlias:    optionalStringFromPtr(w.TargetModelAlias, prior.TargetModelAlias),
		TargetMcpServerID:   optionalStringFromPtr(w.TargetMcpServerID, prior.TargetMcpServerID),
	}
}

// llmTokenBudgetUpdateRequest mirrors the llm-gateway UpdateBudgetRequest
// body. Omitted keys leave the corresponding column unchanged, so Update
// sends every managed field. The scope attributes, member_of_group, currency
// and targets are absent by design: the API cannot update them (they are
// RequiresReplace in the schema).
type llmTokenBudgetUpdateRequest struct {
	Name            *string  `json:"name,omitempty"`
	TokenLimit      *int64   `json:"token_limit,omitempty"`
	CostLimit       *float64 `json:"cost_limit,omitempty"`
	AlertThresholds []int64  `json:"alert_thresholds,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
	Period          *string  `json:"period,omitempty"`
	ActionOnExhaust *string  `json:"action_on_exhaust,omitempty"`
	TrafficType     *string  `json:"traffic_type,omitempty"`
	ChangeNote      *string  `json:"change_note,omitempty"`
}

// llmTokenBudgetResponse mirrors the ai_governance TokenBudget response.
type llmTokenBudgetResponse struct {
	ID              string   `json:"id"`
	OrgID           string   `json:"org_id"`
	Name            string   `json:"name"`
	ScopeType       string   `json:"scope_type"`
	ScopeID         *string  `json:"scope_id"`
	ScopeValue      *string  `json:"scope_value"`
	MemberOfGroup   *string  `json:"member_of_group"`
	Period          string   `json:"period"`
	TokenLimit      int64    `json:"token_limit"`
	CostLimit       *float64 `json:"cost_limit"`
	Currency        string   `json:"currency"`
	AlertThresholds []int64  `json:"alert_thresholds"`
	ActionOnExhaust string   `json:"action_on_exhaust"`
	TrafficType     string   `json:"traffic_type"`
	Enabled         bool     `json:"enabled"`
	CreatedAt       *string  `json:"created_at"`
	llmTargetsWire
}

// int64sFromList converts a list of ints to a Go slice; null/unknown convert
// to nil (key omitted, server default applies).
func int64sFromList(ctx context.Context, l types.List) ([]int64, error) {
	if l.IsNull() || l.IsUnknown() {
		return nil, nil
	}
	out := []int64{}
	if diags := l.ElementsAs(ctx, &out, false); diags.HasError() {
		return nil, fmt.Errorf("%v", diags)
	}
	return out, nil
}

// applyLlmTokenBudgetResponse maps the server's view onto a state model.
// prior is the plan (Create/Update) or previous state (Read), used to settle
// cleared optionals back to null.
func applyLlmTokenBudgetResponse(ctx context.Context, budget *llmTokenBudgetResponse, prior *llmTokenBudgetResourceModel) (llmTokenBudgetResourceModel, diag.Diagnostics) {
	thresholds, diags := types.ListValueFrom(ctx, types.Int64Type, budget.AlertThresholds)

	// token_limit 0 is the cost-only shape: configuration cannot say 0
	// (AtLeast(1)), so it reads back as unset.
	tokenLimit := types.Int64Null()
	if budget.TokenLimit != 0 {
		tokenLimit = types.Int64Value(budget.TokenLimit)
	}
	costLimit := types.Float64Null()
	if budget.CostLimit != nil {
		costLimit = types.Float64Value(*budget.CostLimit)
	}
	currency := budget.Currency
	if currency == "" {
		currency = "USD"
	}

	return llmTokenBudgetResourceModel{
		ID:              types.StringValue(budget.ID),
		OrgID:           types.StringValue(budget.OrgID),
		Name:            types.StringValue(budget.Name),
		ScopeType:       types.StringValue(budget.ScopeType),
		ScopeID:         optionalStringFromPtr(budget.ScopeID, prior.ScopeID),
		ScopeValue:      optionalStringFromPtr(budget.ScopeValue, prior.ScopeValue),
		MemberOfGroup:   optionalStringFromPtr(budget.MemberOfGroup, prior.MemberOfGroup),
		Period:          types.StringValue(budget.Period),
		TokenLimit:      tokenLimit,
		CostLimit:       costLimit,
		Currency:        types.StringValue(currency),
		AlertThresholds: thresholds,
		ActionOnExhaust: types.StringValue(budget.ActionOnExhaust),
		TrafficType:     types.StringValue(budget.TrafficType),
		Enabled:         types.BoolValue(budget.Enabled),
		// change_note is write-only: it annotates the last write, so it
		// settles from the plan (or prior state on Read).
		ChangeNote:      prior.ChangeNote,
		llmTargetsModel: budget.model(prior.llmTargetsModel),
		CreatedAt:       optionalStringFromPtr(budget.CreatedAt, prior.CreatedAt),
	}, diags
}
