// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
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

// llmPricingSyncModes are the per-row sync behaviors of a pricing rule
// (bdai-platform migration V33): `tracking` surfaces Barndoor default updates
// for admin review, `pinned` is org-managed (never touched by any sync), and
// `auto` is overwritten in place whenever the Barndoor default moves.
var llmPricingSyncModes = []string{"tracking", "pinned", "auto"}

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &llmModelPricingResource{}
	_ resource.ResourceWithConfigure   = &llmModelPricingResource{}
	_ resource.ResourceWithImportState = &llmModelPricingResource{}
)

// NewLlmModelPricingResource returns a new barndoor_llm_model_pricing
// resource.
func NewLlmModelPricingResource() resource.Resource {
	return &llmModelPricingResource{}
}

// llmModelPricingResource manages an LLM Gateway model pricing rule through
// the llm-gateway-service admin REST API (`/api/llm-gateway/admin/
// model-pricing`).
//
// # How Terraform maps onto the platform's versioned pricing store
//
// Server-side, pricing is APPEND-ONLY and VERSIONED (bdai-platform migrations
// V34/V35): a logical rule is the group of all rows sharing
// `(org, model_pattern, model_provider)`, every price change appends a new
// version row, the row with the latest `effective_from <= now()` is the one
// the cost calculator bills with, future-dated rows are scheduled changes,
// and "deletion" appends an `is_archived` tombstone version (restorable, full
// history preserved).
//
// This resource therefore manages the CURRENT PRICING INTENT of one logical
// rule, not one immutable row:
//
//   - Create POSTs the first version (`change_source = admin_create`). It
//     refuses to adopt a rule that already has a live price — import that
//     instead — but happily resurrects an archived rule.
//   - Update POSTs a new version (`admin_edit`, or `admin_schedule` when
//     `effective_from` is future-dated); the server keeps the full history.
//     The one exception: while the version Terraform manages is itself still
//     future-dated (a pending scheduled change), Update edits it in place via
//     PUT — the platform allows in-place edits of not-yet-effective versions
//     only, and POSTing a second version at the same instant would violate
//     the store's uniqueness on `(rule, effective_from)`.
//   - Read resolves the rule's currently-effective version (or the pending
//     scheduled version Terraform created, until it activates) and treats an
//     out-of-band archive as "gone" so the next plan recreates the rule.
//   - Delete archives the whole rule (tombstone + cancel pending scheduled
//     changes); it never hard-deletes history.
//
// `id` tracks the version row currently backing the resource, so it CHANGES
// on price updates — that is inherent to the append-only store, not drift.
type llmModelPricingResource struct {
	client *client.Client
}

// llmModelPricingResourceModel maps the resource schema to Go types.
type llmModelPricingResourceModel struct {
	ID            types.String  `tfsdk:"id"`
	OrgID         types.String  `tfsdk:"org_id"`
	ModelPattern  types.String  `tfsdk:"model_pattern"`
	ModelProvider types.String  `tfsdk:"model_provider"`
	ProviderID    types.String  `tfsdk:"provider_id"`
	CatalogSlug   types.String  `tfsdk:"catalog_slug"`
	InputCost     types.Float64 `tfsdk:"input_cost_per_million_tokens"`
	OutputCost    types.Float64 `tfsdk:"output_cost_per_million_tokens"`
	CacheRead     types.Float64 `tfsdk:"cache_read_cost_per_million_tokens"`
	CacheWrite    types.Float64 `tfsdk:"cache_write_cost_per_million_tokens"`
	SyncMode      types.String  `tfsdk:"sync_mode"`
	EffectiveFrom types.String  `tfsdk:"effective_from"`
	ChangeReason  types.String  `tfsdk:"change_reason"`
	ChangeSource  types.String  `tfsdk:"change_source"`
}

func (r *llmModelPricingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_model_pricing"
}

func (r *llmModelPricingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway model pricing rule: the per-million-token costs " +
			"the platform bills against token budgets for models matching `model_pattern`.\n\n" +
			"**The platform stores pricing as an append-only version history** — a rule is identified by " +
			"`(model_provider, model_pattern)`, every price change appends a new version (the version " +
			"with the latest `effective_from` in the past is the billed one), and deletion archives the " +
			"rule with a restorable tombstone. This resource manages the rule's **current pricing " +
			"intent**: updates append new versions (so `id` changes and history accumulates on the " +
			"platform — that is by design, not drift), a future-dated `effective_from` schedules the " +
			"change, and `terraform destroy` **archives** the rule (history preserved, restorable in " +
			"the app) rather than deleting it.\n\n" +
			"Create refuses to silently take over a rule that already has a live price — use " +
			"`terraform import` for those. Recreating over an archived rule resurrects it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "UUID of the pricing **version row** currently backing this " +
					"resource. Because every update appends a new version, this changes on updates.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					llmPricingVersionAttributePlanModifier{},
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
			"model_pattern": schema.StringAttribute{
				MarkdownDescription: "Model name or glob pattern the rule prices (e.g. `gpt-4o` or " +
					"`claude-*`). Half of the rule's identity — changing it forces a new resource " +
					"(archiving the old rule).",
				Required: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"model_provider": schema.StringAttribute{
				MarkdownDescription: "Model-provider scope of the rule (e.g. `openai`) — the other half " +
					"of the rule's identity; unset matches any provider. When unset but `catalog_slug` " +
					"is set, the platform persists the catalog slug here (mirroring its import " +
					"semantics), and this attribute tracks that value. Changing it forces a new resource.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"provider_id": schema.StringAttribute{
				MarkdownDescription: "Optional UUID of a specific LLM Gateway provider instance the " +
					"rule is associated with. The platform's in-place edit cannot change it, so " +
					"changing it forces a new resource.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"catalog_slug": schema.StringAttribute{
				MarkdownDescription: "Optional provider-catalog slug (e.g. `openai`, `aws_bedrock`) for " +
					"provider-level pricing. The platform's in-place edit cannot change it, so changing " +
					"it forces a new resource.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"input_cost_per_million_tokens": schema.Float64Attribute{
				MarkdownDescription: "Cost billed per million input (prompt) tokens.",
				Required:            true,
				Validators: []validator.Float64{
					float64validator.AtLeast(0),
				},
			},
			"output_cost_per_million_tokens": schema.Float64Attribute{
				MarkdownDescription: "Cost billed per million output (completion) tokens.",
				Required:            true,
				Validators: []validator.Float64{
					float64validator.AtLeast(0),
				},
			},
			"cache_read_cost_per_million_tokens": schema.Float64Attribute{
				MarkdownDescription: "Cost billed per million input tokens served from the prompt " +
					"cache. Unset means no override: the platform bills cached input at the full " +
					"input rate (or a Barndoor default when one applies).",
				Optional: true,
				Validators: []validator.Float64{
					float64validator.AtLeast(0),
				},
			},
			"cache_write_cost_per_million_tokens": schema.Float64Attribute{
				MarkdownDescription: "Cost billed per million input tokens written to the prompt cache " +
					"(the cache-creation premium of Anthropic-family models). Unset means no override.",
				Optional: true,
				Validators: []validator.Float64{
					float64validator.AtLeast(0),
				},
			},
			"sync_mode": schema.StringAttribute{
				MarkdownDescription: "How the rule reacts to Barndoor's default pricing updates: " +
					"`pinned` (org-managed, never touched by any sync — the platform default for " +
					"admin-created rules and the recommended mode under Terraform, since `tracking`/" +
					"`auto` let the platform change the price out from under the configuration), " +
					"`tracking` (surfaces default updates for in-app review), or `auto` (a background " +
					"task rewrites the price whenever the Barndoor default moves).",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.OneOf(llmPricingSyncModes...),
				},
			},
			"effective_from": schema.StringAttribute{
				MarkdownDescription: "When the configured price takes effect (RFC 3339). Unset means " +
					"\"now\": the platform stamps the write time and this attribute tracks it. Set a " +
					"**future** timestamp to schedule the price change (`change_source = " +
					"admin_schedule`); until it activates, the resource tracks the scheduled version. " +
					"After a scheduled change activates, remove the attribute (or move it forward with " +
					"the next price change) — re-applying an already-past timestamp alongside a price " +
					"change is rejected, because the platform's version store is keyed by " +
					"`(rule, effective_from)` and past versions are immutable.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					llmPricingRFC3339Validator{},
				},
				PlanModifiers: []planmodifier.String{
					llmPricingEffectiveFromPlanModifier{},
				},
			},
			"change_reason": schema.StringAttribute{
				MarkdownDescription: "Optional free-text note (\"Q1 pricing review\", \"Negotiated " +
					"discount\") stamped onto the **next version this resource writes** and shown on " +
					"the platform's pricing history timeline. Write-only metadata: it is not refreshed " +
					"from the platform, and changing it alone does not create a new version.",
				Optional: true,
			},
			"change_source": schema.StringAttribute{
				MarkdownDescription: "How the platform recorded the version backing this resource: " +
					"`admin_create` for a rule's first version, `admin_edit` for an immediate price " +
					"change, `admin_schedule` for a future-dated one.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					llmPricingVersionAttributePlanModifier{},
				},
			},
		},
	}
}

func (r *llmModelPricingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmModelPricingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmModelPricingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Refuse to silently take over a rule that already has a live price: a
	// POST would append an `admin_edit` version on top of somebody else's
	// rule with no warning. An archived rule (or one with no history) is
	// fair game — recreating over a tombstone is exactly how an out-of-band
	// archive is reconciled.
	//
	// The group key mirrors the platform's fallback: an unset model_provider
	// with a catalog_slug persists the slug as the provider scope.
	groupProvider := plan.ModelProvider
	if groupProvider.IsNull() || groupProvider.IsUnknown() {
		groupProvider = plan.CatalogSlug
	}
	stack, err := r.fetchPricingHistory(ctx, plan.ModelPattern.ValueString(), groupProvider)
	if err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model pricing rule",
			"check for an existing pricing rule before create", err)
		return
	}
	if current := llmPricingCurrentVersion(stack); current != nil && !current.IsArchived {
		resp.Diagnostics.AddError(
			"LLM model pricing rule already exists",
			fmt.Sprintf("The platform already has a live pricing rule for model_pattern %q"+
				" (model_provider %s). Creating this resource would silently append a new version on"+
				" top of it. Import the existing rule instead:\n\n  terraform import <address> %q",
				plan.ModelPattern.ValueString(), llmPricingProviderLabel(groupProvider),
				llmPricingImportID(groupProvider, plan.ModelPattern.ValueString())),
		)
		return
	}

	body := llmPricingCreateBodyFromPlan(&plan)
	if v, ok := knownString(plan.EffectiveFrom); ok {
		body.EffectiveFrom = &v
	}

	var version llmPricingVersionResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/model-pricing", body, &version); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model pricing rule",
			"create the LLM model pricing rule", err)
		return
	}
	if version.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no version id.")
		return
	}

	newState := applyLlmPricingVersionResponse(&version, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Read resolves the version this resource tracks out of the rule's history
// stack: the pending scheduled version Terraform wrote (matched by state id,
// while it is still future-dated), otherwise the currently-effective version.
// A missing group, or a group whose current version is an archive tombstone,
// drops the resource so Terraform plans a recreate.
func (r *llmModelPricingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmModelPricingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	stack, err := r.fetchPricingHistory(ctx, state.ModelPattern.ValueString(), state.ModelProvider)
	if err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model pricing rule",
			"read the LLM model pricing rule's history", err)
		return
	}

	managed := llmPricingManagedVersion(stack, state.ID.ValueString())
	if managed == nil {
		// No history, or the rule was archived out-of-band: plan a recreate.
		resp.State.RemoveResource(ctx)
		return
	}

	newState := applyLlmPricingVersionResponse(managed, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *llmModelPricingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan, state, config llmModelPricingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// While the version we manage is itself still future-dated (a pending
	// scheduled change), edit it in place: the platform allows PUT on
	// not-yet-effective versions only, and a POST would collide with it on
	// the store's (rule, effective_from) uniqueness.
	if r.stateVersionIsScheduled(ctx, &state, &resp.Diagnostics) {
		if resp.Diagnostics.HasError() {
			return
		}
		r.updateScheduledInPlace(ctx, &plan, &state, resp)
		return
	}

	// Standard path: append a new version. effective_from is forwarded only
	// when the CONFIGURATION pins it — a computed value carried from state is
	// the *previous* version's timestamp, and re-posting it would collide
	// with that version.
	body := llmPricingCreateBodyFromPlan(&plan)
	if cfgEff, ok := knownString(config.EffectiveFrom); ok {
		if t, err := llmPricingParseTime(cfgEff); err == nil && !t.After(time.Now()) {
			if stateEff, ok := knownString(state.EffectiveFrom); ok && llmPricingSameInstant(cfgEff, stateEff) {
				resp.Diagnostics.AddAttributeError(
					path.Root("effective_from"),
					"Stale effective_from on a pricing update",
					"effective_from is pinned to the already-active timestamp of the current version, "+
						"and the platform's pricing versions are immutable once effective — a new "+
						"version cannot reuse that instant. Remove effective_from (the platform stamps "+
						"the write time) or set a future timestamp to schedule the change.",
				)
				return
			}
		}
		body.EffectiveFrom = &cfgEff
	}

	var version llmPricingVersionResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/model-pricing", body, &version); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model pricing rule",
			"update the LLM model pricing rule", err)
		return
	}

	newState := applyLlmPricingVersionResponse(&version, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete archives the rule: the platform's DELETE on the currently-effective
// version appends an is_archived tombstone and cancels any pending scheduled
// versions. When the rule has no effective version yet (Terraform only ever
// scheduled its first price), the scheduled versions are cancelled instead,
// which removes the rule outright.
func (r *llmModelPricingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmModelPricingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	stack, err := r.fetchPricingHistory(ctx, state.ModelPattern.ValueString(), state.ModelProvider)
	if err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model pricing rule",
			"read the LLM model pricing rule's history before archiving", err)
		return
	}
	if len(stack) == 0 {
		return // already gone
	}

	if current := llmPricingCurrentVersion(stack); current != nil {
		if current.IsArchived {
			return // already archived out-of-band
		}
		if err := doJSON(ctx, r.client, http.MethodDelete,
			llmGatewayAPIPrefix+"/model-pricing/"+current.ID, nil, nil); err != nil && !isNotFound(err) {
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM model pricing rule",
				"archive the LLM model pricing rule", err)
		}
		return
	}

	// Only future-dated versions exist: cancel each scheduled version.
	for i := range stack {
		if err := doJSON(ctx, r.client, http.MethodDelete,
			llmGatewayAPIPrefix+"/model-pricing/"+stack[i].ID, nil, nil); err != nil && !isNotFound(err) {
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM model pricing rule",
				"cancel the LLM model pricing rule's scheduled versions", err)
			return
		}
	}
}

// ImportState imports a rule by its logical identity, not a version UUID
// (version ids rotate on every change): either `model_pattern` alone for a
// provider-unscoped rule, or `model_provider|model_pattern`. The provider
// scope comes first because model patterns may contain any character (Bedrock
// model ids contain `:`), while provider scopes are simple slugs.
func (r *llmModelPricingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := strings.TrimSpace(req.ID)
	provider, pattern, scoped := strings.Cut(id, "|")
	if !scoped {
		provider, pattern = "", id
	}
	if pattern == "" {
		resp.Diagnostics.AddError(
			"Invalid LLM model pricing import ID",
			fmt.Sprintf("Expected `model_pattern` or `model_provider|model_pattern`, got %q.", req.ID),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("model_pattern"), pattern)...)
	if scoped {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("model_provider"), provider)...)
	}
}

// stateVersionIsScheduled reports whether the version row recorded in state
// still exists and is future-dated (i.e. Terraform manages a pending
// scheduled change). Non-404 lookup failures are surfaced as diagnostics.
func (r *llmModelPricingResource) stateVersionIsScheduled(ctx context.Context, state *llmModelPricingResourceModel, diags *diag.Diagnostics) bool {
	id, ok := knownString(state.ID)
	if !ok || id == "" {
		return false
	}
	var version llmPricingVersionResponse
	if err := doJSON(ctx, r.client, http.MethodGet,
		llmGatewayAPIPrefix+"/model-pricing/version/"+id, nil, &version); err != nil {
		if isNotFound(err) {
			return false
		}
		addLlmGatewayAPIError(diags, "LLM model pricing rule",
			"look up the LLM model pricing version before updating", err)
		return false
	}
	t, err := llmPricingParseTime(version.EffectiveFrom)
	return err == nil && t.After(time.Now())
}

// updateScheduledInPlace PUTs the plan onto the still-pending scheduled
// version. The cache-cost keys are always present in the body (explicit null
// clears — the endpoint's double-Option semantics), everything else is
// COALESCE.
func (r *llmModelPricingResource) updateScheduledInPlace(ctx context.Context, plan, state *llmModelPricingResourceModel, resp *resource.UpdateResponse) {
	input := plan.InputCost.ValueFloat64()
	output := plan.OutputCost.ValueFloat64()
	body := &llmPricingUpdateRequest{
		InputCost:  &input,
		OutputCost: &output,
		CacheRead:  float64PtrFromFloat64(plan.CacheRead),
		CacheWrite: float64PtrFromFloat64(plan.CacheWrite),
	}
	if v, ok := knownString(plan.EffectiveFrom); ok {
		body.EffectiveFrom = &v
	}
	if v, ok := knownString(plan.SyncMode); ok {
		body.SyncMode = &v
	}
	if v, ok := knownString(plan.ChangeReason); ok {
		body.ChangeReason = &v
	}

	var version llmPricingVersionResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/model-pricing/"+state.ID.ValueString(), body, &version); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM model pricing rule",
			"update the scheduled LLM model pricing version", err)
		return
	}

	newState := applyLlmPricingVersionResponse(&version, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// fetchPricingHistory returns the rule's full version stack (newest
// effective_from first). modelProvider null/unknown targets rules without a
// provider scope, matching the platform's COALESCE(model_provider, ”) key.
func (r *llmModelPricingResource) fetchPricingHistory(ctx context.Context, modelPattern string, modelProvider types.String) ([]llmPricingVersionResponse, error) {
	path := llmGatewayAPIPrefix + "/model-pricing/history?model_pattern=" + url.QueryEscape(modelPattern)
	if v, ok := knownString(modelProvider); ok {
		path += "&model_provider=" + url.QueryEscape(v)
	}
	var stack []llmPricingVersionResponse
	if err := doJSON(ctx, r.client, http.MethodGet, path, nil, &stack); err != nil {
		return nil, err
	}
	return stack, nil
}

func (r *llmModelPricingResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// --- version-stack resolution --------------------------------------------------

// llmPricingCurrentVersion picks the currently-effective version out of a
// stack: the latest effective_from that is not in the future. Returns nil
// when every version is future-dated (or the stack is empty). Timestamps
// that fail to parse count as effective, erring towards "the rule exists".
func llmPricingCurrentVersion(stack []llmPricingVersionResponse) *llmPricingVersionResponse {
	now := time.Now()
	var current *llmPricingVersionResponse
	var currentAt time.Time
	for i := range stack {
		t, err := llmPricingParseTime(stack[i].EffectiveFrom)
		if err == nil && t.After(now) {
			continue
		}
		if current == nil || t.After(currentAt) {
			current = &stack[i]
			currentAt = t
		}
	}
	return current
}

// llmPricingManagedVersion resolves the version a resource tracks: the
// still-pending scheduled version it wrote (matched by state id), otherwise
// the currently-effective version — nil when the rule has no live version or
// was archived out-of-band.
func llmPricingManagedVersion(stack []llmPricingVersionResponse, stateID string) *llmPricingVersionResponse {
	if stateID != "" {
		now := time.Now()
		for i := range stack {
			if stack[i].ID != stateID {
				continue
			}
			if t, err := llmPricingParseTime(stack[i].EffectiveFrom); err == nil && t.After(now) {
				return &stack[i]
			}
			break
		}
	}
	current := llmPricingCurrentVersion(stack)
	if current == nil || current.IsArchived {
		return nil
	}
	return current
}

// llmPricingImportID renders the import ID for a rule's logical identity.
func llmPricingImportID(modelProvider types.String, modelPattern string) string {
	if v, ok := knownString(modelProvider); ok {
		return v + "|" + modelPattern
	}
	return modelPattern
}

// llmPricingProviderLabel renders a provider scope for diagnostics.
func llmPricingProviderLabel(modelProvider types.String) string {
	if v, ok := knownString(modelProvider); ok {
		return fmt.Sprintf("%q", v)
	}
	return "unset"
}

// --- time helpers ----------------------------------------------------------------

// llmPricingParseTime parses the platform's RFC 3339 timestamps (chrono emits
// fractional seconds when non-zero).
func llmPricingParseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// llmPricingSameInstant reports whether two RFC 3339 strings denote the same
// instant (the platform may echo a configured timestamp in a different
// textual form, e.g. `+00:00` vs `Z`).
func llmPricingSameInstant(a, b string) bool {
	ta, errA := llmPricingParseTime(a)
	tb, errB := llmPricingParseTime(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ta.Equal(tb)
}

// --- plan modifiers ---------------------------------------------------------------

// llmPricingVersionWriteChanges reports whether the planned change writes a
// new version server-side (anything except a change_reason-only edit, which
// the platform's skip-on-no-op absorbs without inserting).
func llmPricingVersionWriteChanges(plan, state *llmModelPricingResourceModel) bool {
	return !plan.InputCost.Equal(state.InputCost) ||
		!plan.OutputCost.Equal(state.OutputCost) ||
		!plan.CacheRead.Equal(state.CacheRead) ||
		!plan.CacheWrite.Equal(state.CacheWrite) ||
		!plan.SyncMode.Equal(state.SyncMode) ||
		!plan.EffectiveFrom.Equal(state.EffectiveFrom)
}

// llmPricingVersionAttributePlanModifier marks a version-scoped computed
// attribute (id, change_source) unknown when the update will write a new
// version — the append-only store rotates them — and keeps the state value
// for no-op-shaped updates (change_reason-only).
type llmPricingVersionAttributePlanModifier struct{}

func (llmPricingVersionAttributePlanModifier) Description(_ context.Context) string {
	return "Value rotates when the update appends a new pricing version."
}

func (m llmPricingVersionAttributePlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (llmPricingVersionAttributePlanModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy: the framework already handles unknowns
	}
	var plan, state llmModelPricingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if llmPricingVersionWriteChanges(&plan, &state) {
		resp.PlanValue = types.StringUnknown()
	}
}

// llmPricingEffectiveFromPlanModifier handles the computed side of
// effective_from: when the configuration does not pin it and the update
// writes a new version, the platform stamps a fresh timestamp, so the planned
// value must be unknown rather than the previous version's timestamp.
type llmPricingEffectiveFromPlanModifier struct{}

func (llmPricingEffectiveFromPlanModifier) Description(_ context.Context) string {
	return "The platform stamps the write time when the configuration does not pin effective_from."
}

func (m llmPricingEffectiveFromPlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (llmPricingEffectiveFromPlanModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || !req.ConfigValue.IsNull() {
		return
	}
	var plan, state llmModelPricingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// effective_from itself is excluded: with a null config it is carried
	// from state, so it never differs on its own here.
	if !plan.InputCost.Equal(state.InputCost) ||
		!plan.OutputCost.Equal(state.OutputCost) ||
		!plan.CacheRead.Equal(state.CacheRead) ||
		!plan.CacheWrite.Equal(state.CacheWrite) ||
		!plan.SyncMode.Equal(state.SyncMode) {
		resp.PlanValue = types.StringUnknown()
	}
}

// llmPricingRFC3339Validator validates that a configured effective_from
// parses as RFC 3339.
type llmPricingRFC3339Validator struct{}

func (llmPricingRFC3339Validator) Description(_ context.Context) string {
	return "value must be an RFC 3339 timestamp"
}

func (v llmPricingRFC3339Validator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (llmPricingRFC3339Validator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := llmPricingParseTime(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid RFC 3339 timestamp",
			fmt.Sprintf("effective_from must be an RFC 3339 timestamp (e.g. 2030-01-01T00:00:00Z): %s", err),
		)
	}
}

// --- request/response DTOs ---------------------------------------------------

// llmPricingCreateRequest mirrors the llm-gateway CreatePricingRequest body.
// sync_mode is omitted when unset — the platform defaults admin-authored rows
// to `pinned`.
type llmPricingCreateRequest struct {
	ModelProvider *string  `json:"model_provider,omitempty"`
	ModelPattern  string   `json:"model_pattern"`
	InputCost     float64  `json:"input_cost_per_million_tokens"`
	OutputCost    float64  `json:"output_cost_per_million_tokens"`
	EffectiveFrom *string  `json:"effective_from,omitempty"`
	ProviderID    *string  `json:"provider_id,omitempty"`
	CatalogSlug   *string  `json:"catalog_slug,omitempty"`
	SyncMode      *string  `json:"sync_mode,omitempty"`
	ChangeReason  *string  `json:"change_reason,omitempty"`
	CacheRead     *float64 `json:"cache_read_cost_per_million_tokens,omitempty"`
	CacheWrite    *float64 `json:"cache_write_cost_per_million_tokens,omitempty"`
}

// llmPricingUpdateRequest mirrors the llm-gateway UpdatePricingRequest body
// (the in-place edit of a not-yet-effective scheduled version). The two
// cache-cost keys are deliberately **not** omitempty: the endpoint's
// double-Option semantics distinguish an absent key (keep the current value)
// from an explicit null (clear the override), and Terraform's plan is the
// full desired state — a nil pointer must clear.
type llmPricingUpdateRequest struct {
	InputCost     *float64 `json:"input_cost_per_million_tokens,omitempty"`
	OutputCost    *float64 `json:"output_cost_per_million_tokens,omitempty"`
	EffectiveFrom *string  `json:"effective_from,omitempty"`
	SyncMode      *string  `json:"sync_mode,omitempty"`
	ChangeReason  *string  `json:"change_reason,omitempty"`
	CacheRead     *float64 `json:"cache_read_cost_per_million_tokens"`
	CacheWrite    *float64 `json:"cache_write_cost_per_million_tokens"`
}

// llmPricingVersionResponse mirrors the llm-gateway AdminModelPricing wire
// shape: the flattened ai_governance ModelPricing plus the V33/V34/V35
// versioning metadata. The actor fields (created_by_user_id/email) and
// provider_name are decoded but not surfaced as attributes.
type llmPricingVersionResponse struct {
	ID            string   `json:"id"`
	OrgID         string   `json:"org_id"`
	ModelProvider *string  `json:"model_provider"`
	ModelPattern  string   `json:"model_pattern"`
	InputCost     float64  `json:"input_cost_per_million_tokens"`
	OutputCost    float64  `json:"output_cost_per_million_tokens"`
	EffectiveFrom string   `json:"effective_from"`
	ProviderID    *string  `json:"provider_id"`
	CatalogSlug   *string  `json:"catalog_slug"`
	CacheRead     *float64 `json:"cache_read_cost_per_million_tokens"`
	CacheWrite    *float64 `json:"cache_write_cost_per_million_tokens"`
	SyncMode      string   `json:"sync_mode"`
	ChangeSource  string   `json:"change_source"`
	ChangeReason  *string  `json:"change_reason"`
	IsArchived    bool     `json:"is_archived"`
}

// llmPricingCreateBodyFromPlan builds the POST body shared by Create and the
// new-version Update path. effective_from is left for the caller — its
// semantics differ between the two.
func llmPricingCreateBodyFromPlan(plan *llmModelPricingResourceModel) *llmPricingCreateRequest {
	body := &llmPricingCreateRequest{
		ModelPattern: plan.ModelPattern.ValueString(),
		InputCost:    plan.InputCost.ValueFloat64(),
		OutputCost:   plan.OutputCost.ValueFloat64(),
		CacheRead:    float64PtrFromFloat64(plan.CacheRead),
		CacheWrite:   float64PtrFromFloat64(plan.CacheWrite),
	}
	if v, ok := knownString(plan.ModelProvider); ok {
		body.ModelProvider = &v
	}
	if v, ok := knownString(plan.ProviderID); ok {
		body.ProviderID = &v
	}
	if v, ok := knownString(plan.CatalogSlug); ok {
		body.CatalogSlug = &v
	}
	if v, ok := knownString(plan.SyncMode); ok {
		body.SyncMode = &v
	}
	if v, ok := knownString(plan.ChangeReason); ok {
		body.ChangeReason = &v
	}
	return body
}

// applyLlmPricingVersionResponse maps the platform's view of a version onto a
// state model. prior is the plan (Create/Update) or previous state (Read):
// change_reason always settles from it (write-only metadata — a no-op write
// echoes the previous version's reason), and a configured effective_from
// string is kept verbatim when it denotes the same instant the platform
// echoed back.
func applyLlmPricingVersionResponse(version *llmPricingVersionResponse, prior *llmModelPricingResourceModel) llmModelPricingResourceModel {
	effectiveFrom := types.StringValue(version.EffectiveFrom)
	if v, ok := knownString(prior.EffectiveFrom); ok && llmPricingSameInstant(v, version.EffectiveFrom) {
		effectiveFrom = prior.EffectiveFrom
	}
	return llmModelPricingResourceModel{
		ID:            types.StringValue(version.ID),
		OrgID:         types.StringValue(version.OrgID),
		ModelPattern:  types.StringValue(version.ModelPattern),
		ModelProvider: stringFromPtr(version.ModelProvider),
		ProviderID:    stringFromPtr(version.ProviderID),
		CatalogSlug:   stringFromPtr(version.CatalogSlug),
		InputCost:     types.Float64Value(version.InputCost),
		OutputCost:    types.Float64Value(version.OutputCost),
		CacheRead:     float64FromPtr(version.CacheRead),
		CacheWrite:    float64FromPtr(version.CacheWrite),
		SyncMode:      types.StringValue(version.SyncMode),
		EffectiveFrom: effectiveFrom,
		ChangeReason:  prior.ChangeReason,
		ChangeSource:  types.StringValue(version.ChangeSource),
	}
}

// --- small value helpers -------------------------------------------------------

// float64PtrFromFloat64 converts a known types.Float64 to a wire pointer; a
// null/unknown value converts to nil (key omitted / JSON null).
func float64PtrFromFloat64(v types.Float64) *float64 {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	f := v.ValueFloat64()
	return &f
}

// float64FromPtr maps an optional wire float to state: nil means null.
func float64FromPtr(p *float64) types.Float64 {
	if p == nil {
		return types.Float64Null()
	}
	return types.Float64Value(*p)
}
