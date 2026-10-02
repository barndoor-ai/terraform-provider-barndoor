// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &llmRoutingPolicyResource{}
	_ resource.ResourceWithConfigure      = &llmRoutingPolicyResource{}
	_ resource.ResourceWithImportState    = &llmRoutingPolicyResource{}
	_ resource.ResourceWithValidateConfig = &llmRoutingPolicyResource{}
)

// Platform draft defaults (`RoutingPolicyDraft` serde defaults) and bounds
// (`validate_draft`). The provider materializes the defaults as schema
// defaults, so the configuration is the full desired state on every write.
const (
	llmRoutingDefaultPosture             = "balanced"
	llmRoutingDefaultRouterInputMaxChars = 12000
	llmRoutingDefaultSlotOnFailure       = 1
	llmRoutingMaxDeterminerPromptChars   = 8000
	llmRoutingMaxSlotDescriptionChars    = 2000
)

// llmRoutingDefaultBreakpoints is the platform's default_context_breakpoints.
// Two breakpoints split requests into three context bands, so the default
// only fits a three-slot policy.
var llmRoutingDefaultBreakpoints = []int64{128000, 512000}

// llmRoutingPostures are the `Posture` enum values (snake_case on the wire).
var llmRoutingPostures = []string{"savings", "balanced", "quality"}

// NewLlmRoutingPolicyResource returns a new barndoor_llm_routing_policy resource.
func NewLlmRoutingPolicyResource() resource.Resource {
	return &llmRoutingPolicyResource{}
}

// llmRoutingPolicyResource manages an LLM Gateway routing policy through the
// llm-gateway-service admin REST API (`/api/llm-gateway/admin/routing-policies`).
type llmRoutingPolicyResource struct {
	client *client.Client
}

// llmRoutingPolicyResourceModel maps the resource schema to Go types. Slots
// and breakpoints stay framework lists so a partially unknown configuration
// can still be decoded at validation time.
type llmRoutingPolicyResourceModel struct {
	ID                   types.String `tfsdk:"id"`
	OrgID                types.String `tfsdk:"org_id"`
	ModelAlias           types.String `tfsdk:"model_alias"`
	Description          types.String `tfsdk:"description"`
	Enabled              types.Bool   `tfsdk:"enabled"`
	DeterminerModelAlias types.String `tfsdk:"determiner_model_alias"`
	DeterminerPrompt     types.String `tfsdk:"determiner_prompt"`
	Posture              types.String `tfsdk:"posture"`
	Slots                types.List   `tfsdk:"slots"`
	ContextBreakpoints   types.List   `tfsdk:"context_breakpoints"`
	RouterInputMaxChars  types.Int64  `tfsdk:"router_input_max_chars"`
	DefaultSlotOnFailure types.Int64  `tfsdk:"default_slot_on_failure"`
	CreatedAt            types.String `tfsdk:"created_at"`
	UpdatedAt            types.String `tfsdk:"updated_at"`
}

// llmRoutingSlotModel is one element of the slots list.
type llmRoutingSlotModel struct {
	ModelAlias  types.String `tfsdk:"model_alias"`
	Label       types.String `tfsdk:"label"`
	Description types.String `tfsdk:"description"`
}

var llmRoutingSlotAttrTypes = map[string]attr.Type{
	"model_alias": types.StringType,
	"label":       types.StringType,
	"description": types.StringType,
}

// llmRoutingNoSlash rejects a `/` in the policy alias. The platform accepts
// one, but the request path parses a `/` in the `model` field as
// `<provider>/<model>` before it looks for a policy, so such a policy could
// never be selected.
var llmRoutingNoSlash = stringvalidator.RegexMatches(regexp.MustCompile(`^[^/]*$`),
	"must not contain \"/\": callers select a policy by its bare alias, and a model name "+
		"containing \"/\" is read as <provider>/<model>, so the policy could never be selected")

func (r *llmRoutingPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_routing_policy"
}

func (r *llmRoutingPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	breakpointDefaults := make([]attr.Value, 0, len(llmRoutingDefaultBreakpoints))
	for _, b := range llmRoutingDefaultBreakpoints {
		breakpointDefaults = append(breakpointDefaults, types.Int64Value(b))
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway routing policy: a caller-facing `model_alias` " +
			"that, instead of naming one model, picks one of several model **slots** per request. A " +
			"determiner model reads the request (and any `barndoor_llm_routing_rule` attached to the " +
			"policy) and chooses a slot; `context_breakpoints` force larger-context slots as the " +
			"request grows.\n\n" +
			"List slots from cheapest to strongest: rules and breakpoints refer to slots by index, " +
			"and that ordering makes \"at least slot 1\" mean \"at least the mid tier\". The platform " +
			"does not check it against prices. Each slot `model_alias` must already resolve to " +
			"enabled routes: a bare-callable alias (a custom `barndoor_llm_model_mapping` alias, or a " +
			"1:1 enablement with `bare_alias = true`), or the provider-scoped form " +
			"`<provider name>/<model>` of a 1:1 enablement. The policy's own `model_alias` must not " +
			"already be a model mapping alias. The platform checks both on every write and answers " +
			"with a 400 naming the offending target.\n\n" +
			"Every optional setting has the platform's default as its schema default and is sent on " +
			"every write, so removing one from configuration resets it to that default rather than " +
			"keeping a value set elsewhere. Pair the policy with `require_routing_policy` on " +
			"`barndoor_llm_governance_config` to make callers go through policies.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Routing policy UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the policy belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"model_alias": schema.StringAttribute{
				MarkdownDescription: "Caller-facing alias that selects the policy: callers send it as " +
					"the request `model` (matched case-insensitively). Unique per organization " +
					"(case-insensitive; a duplicate is a 409). Must not contain `/` and must not equal an " +
					"existing model mapping alias.",
				Required: true,
				Validators: []validator.String{
					noSurroundingWhitespace,
					llmRoutingNoSlash,
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-text description shown in the app. Removing it clears it.",
				Optional:            true,
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the policy serves traffic. Defaults to `true`. Requests " +
					"naming a disabled policy's alias are rejected with a 400.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"determiner_model_alias": schema.StringAttribute{
				MarkdownDescription: "Model the gateway asks to pick a slot for each request. Not " +
					"checked for existence when the policy is saved; a determiner without enabled routes " +
					"fails at request time and the policy falls back to `default_slot_on_failure`. Must " +
					"not be the policy's own alias.",
				Required: true,
				Validators: []validator.String{
					noSurroundingWhitespace,
				},
			},
			"determiner_prompt": schema.StringAttribute{
				MarkdownDescription: fmt.Sprintf("Custom instructions for the determiner (at most %d "+
					"characters). Unset or empty uses the platform's built-in prompt; removing it from "+
					"configuration reverts to the built-in prompt.", llmRoutingMaxDeterminerPromptChars),
				Optional: true,
				Validators: []validator.String{
					stringvalidator.UTF8LengthAtMost(llmRoutingMaxDeterminerPromptChars),
				},
			},
			"posture": schema.StringAttribute{
				MarkdownDescription: "How the determiner trades cost against quality: `savings`, " +
					"`balanced`, or `quality`. Defaults to `balanced`.",
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(llmRoutingDefaultPosture),
				Validators: []validator.String{
					stringvalidator.OneOf(llmRoutingPostures...),
				},
			},
			"slots": schema.ListNestedAttribute{
				MarkdownDescription: "Candidate models, cheapest/weakest first (at least two). A slot's " +
					"index is its position in this list, which is what routing rules' `floor_slot` and " +
					"`deny_slots` refer to — reordering slots changes what existing rules mean.",
				Required: true,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(2),
				},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"model_alias": schema.StringAttribute{
							MarkdownDescription: "Target the slot routes to: a bare-callable model alias " +
								"or `<provider name>/<model>`. Must not be the policy's own alias.",
							Required: true,
							Validators: []validator.String{
								noSurroundingWhitespace,
							},
						},
						"label": schema.StringAttribute{
							MarkdownDescription: "Short display name for the slot.",
							Optional:            true,
						},
						"description": schema.StringAttribute{
							MarkdownDescription: fmt.Sprintf("What the slot is good for, read by the "+
								"determiner (at most %d characters).", llmRoutingMaxSlotDescriptionChars),
							Optional: true,
							Validators: []validator.String{
								stringvalidator.UTF8LengthAtMost(llmRoutingMaxSlotDescriptionChars),
							},
						},
					},
				},
			},
			"context_breakpoints": schema.ListAttribute{
				MarkdownDescription: "Ascending request-size thresholds, one fewer than there are " +
					"slots: a request larger than the Nth breakpoint is routed to slot N+1 or above. " +
					"Defaults to `[128000, 512000]`, which only fits a three-slot policy — any other slot " +
					"count must set this explicitly.",
				ElementType: types.Int64Type,
				Optional:    true,
				Computed:    true,
				Default:     listdefault.StaticValue(types.ListValueMust(types.Int64Type, breakpointDefaults)),
				Validators: []validator.List{
					listvalidator.ValueInt64sAre(int64validator.Between(1, 1<<31-1)),
				},
			},
			"router_input_max_chars": schema.Int64Attribute{
				MarkdownDescription: "How many characters of the request the determiner is shown. " +
					"Defaults to `12000`.",
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(llmRoutingDefaultRouterInputMaxChars),
				Validators: []validator.Int64{
					int64validator.Between(1, 1<<31-1),
				},
			},
			"default_slot_on_failure": schema.Int64Attribute{
				MarkdownDescription: "Slot index used when the determiner fails or answers unusably. " +
					"Defaults to `1`; must be a valid index into `slots`.",
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(llmRoutingDefaultSlotOnFailure),
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "Creation timestamp (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "Last-update timestamp (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

// llmRoutingTargetAlias mirrors the platform's target_alias: the alias part of
// a `<provider>/<alias>` target, or the whole target when there is no
// non-empty provider and alias on both sides of the first `/`.
func llmRoutingTargetAlias(target string) string {
	if provider, model, ok := strings.Cut(target, "/"); ok && provider != "" && model != "" {
		return model
	}
	return target
}

// ValidateConfig mirrors the platform's validate_draft cross-field rules on
// the known configuration, with the schema defaults substituted for unset
// attributes (they are what the write will send). Every rule is skipped while
// an input it needs is unknown, leaving the API's 400 as the backstop.
func (r *llmRoutingPolicyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg llmRoutingPolicyResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	policyAlias, aliasKnown := knownString(cfg.ModelAlias)

	if determiner, ok := knownString(cfg.DeterminerModelAlias); ok && aliasKnown &&
		strings.EqualFold(llmRoutingTargetAlias(determiner), policyAlias) {
		resp.Diagnostics.AddAttributeError(path.Root("determiner_model_alias"),
			"Determiner cannot be the routing policy itself",
			fmt.Sprintf("determiner_model_alias %q names the policy's own alias %q (compared "+
				"case-insensitively, ignoring a <provider>/ prefix). Point it at a real model.",
				determiner, policyAlias))
	}

	if cfg.Slots.IsNull() || cfg.Slots.IsUnknown() {
		return
	}
	var slots []llmRoutingSlotModel
	resp.Diagnostics.Append(cfg.Slots.ElementsAs(ctx, &slots, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if aliasKnown {
		for i, slot := range slots {
			target, ok := knownString(slot.ModelAlias)
			if ok && strings.EqualFold(llmRoutingTargetAlias(target), policyAlias) {
				resp.Diagnostics.AddAttributeError(path.Root("slots").AtListIndex(i).AtName("model_alias"),
					"Slot cannot target the routing policy itself",
					fmt.Sprintf("Slot %d targets %q, which is the policy's own alias %q (compared "+
						"case-insensitively, ignoring a <provider>/ prefix) — it would route to itself.",
						i, target, policyAlias))
			}
		}
	}

	// The slot-count rules below need a meaningful count; SizeAtLeast(2)
	// already reports a shorter list.
	n := len(slots)
	if n < 2 {
		return
	}

	switch {
	case cfg.ContextBreakpoints.IsNull():
		if want := len(llmRoutingDefaultBreakpoints); n-1 != want {
			resp.Diagnostics.AddAttributeError(path.Root("context_breakpoints"),
				"context_breakpoints must be set for this slot count",
				fmt.Sprintf("The policy has %d slots, so it needs exactly %d context_breakpoints. The "+
					"platform default [128000, 512000] has %d and only fits a %d-slot policy; set "+
					"context_breakpoints to %d ascending token counts.", n, n-1, want, want+1, n-1))
		}
	case !cfg.ContextBreakpoints.IsUnknown():
		breakpoints := cfg.ContextBreakpoints.Elements()
		if len(breakpoints) != n-1 {
			resp.Diagnostics.AddAttributeError(path.Root("context_breakpoints"),
				"Wrong number of context_breakpoints",
				fmt.Sprintf("context_breakpoints must have exactly %d entries for %d slots (one "+
					"fewer than the slots), got %d.", n-1, n, len(breakpoints)))
		}
		var prev *int64
		for i, v := range breakpoints {
			b, ok := v.(types.Int64)
			if !ok || b.IsNull() || b.IsUnknown() {
				prev = nil
				continue
			}
			cur := b.ValueInt64()
			if prev != nil && cur <= *prev {
				resp.Diagnostics.AddAttributeError(path.Root("context_breakpoints").AtListIndex(i),
					"context_breakpoints must be strictly increasing",
					fmt.Sprintf("Breakpoint %d (%d) must be greater than the one before it (%d).", i, cur, *prev))
			}
			prev = &cur
		}
	}

	defaultSlot := int64(llmRoutingDefaultSlotOnFailure)
	if cfg.DefaultSlotOnFailure.IsUnknown() {
		return
	}
	if !cfg.DefaultSlotOnFailure.IsNull() {
		defaultSlot = cfg.DefaultSlotOnFailure.ValueInt64()
	}
	if defaultSlot >= int64(n) {
		resp.Diagnostics.AddAttributeError(path.Root("default_slot_on_failure"),
			"default_slot_on_failure is not a valid slot index",
			fmt.Sprintf("default_slot_on_failure is %d but the policy has %d slots (indices 0–%d). "+
				"When unset it defaults to %d.", defaultSlot, n, n-1, llmRoutingDefaultSlotOnFailure))
	}
}

func (r *llmRoutingPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmRoutingPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmRoutingPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := buildLlmRoutingPolicyRequest(ctx, &plan, false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policy llmRoutingPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/routing-policies", body, &policy); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM routing policy", "create the LLM routing policy", err)
		return
	}
	if policy.ID == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no routing policy id.")
		return
	}

	state, diags := applyLlmRoutingPolicyResponse(ctx, &policy, &plan)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Read refreshes the policy from the org-wide listing: the admin API has no
// get-by-id endpoint for routing policies.
func (r *llmRoutingPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmRoutingPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policies []llmRoutingPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/routing-policies", nil, &policies); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM routing policy", "list LLM routing policies", err)
		return
	}

	for i := range policies {
		if policies[i].ID == state.ID.ValueString() {
			newState, diags := applyLlmRoutingPolicyResponse(ctx, &policies[i], &state)
			resp.Diagnostics.Append(diags...)
			resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
			return
		}
	}

	// Deleted out-of-band; drop the resource so Terraform plans a recreate.
	resp.State.RemoveResource(ctx)
}

func (r *llmRoutingPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan llmRoutingPolicyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := buildLlmRoutingPolicyRequest(ctx, &plan, true)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	var policy llmRoutingPolicyResponse
	if err := doJSON(ctx, r.client, http.MethodPut,
		llmGatewayAPIPrefix+"/routing-policies/"+plan.ID.ValueString(), body, &policy); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM routing policy", "update the LLM routing policy", err)
		return
	}

	newState, diags := applyLlmRoutingPolicyResponse(ctx, &policy, &plan)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete removes the policy; the platform cascades its routing rules. A 404
// means it is already gone — success for a destroy.
func (r *llmRoutingPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state llmRoutingPolicyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		llmGatewayAPIPrefix+"/routing-policies/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM routing policy", "delete the LLM routing policy", err)
	}
}

func (r *llmRoutingPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmRoutingPolicyResource) requireClient(diags *diag.Diagnostics) bool {
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

// llmRoutingSlotPayload mirrors the platform RoutingSlot.
type llmRoutingSlotPayload struct {
	ModelAlias  string  `json:"model_alias"`
	Label       *string `json:"label,omitempty"`
	Description *string `json:"description,omitempty"`
}

// llmRoutingPolicyRequest is both the create body (RoutingPolicyDraft) and the
// update body (RoutingPolicyUpdate). Every field is sent on both: the update
// is a keep-if-omitted merge, and Terraform's plan is the full desired state.
//
// The two clearable strings are the exception to "omitted means default". The
// update's `Option<Option<String>>` fields read a JSON null as "keep", so
// there is no way to null them; on update an unset value is sent as "" instead
// (an empty determiner_prompt means "use the built-in prompt"), and
// applyLlmRoutingPolicyResponse settles the server's "" back to null.
type llmRoutingPolicyRequest struct {
	ModelAlias           string                  `json:"model_alias"`
	Description          *string                 `json:"description,omitempty"`
	Enabled              bool                    `json:"enabled"`
	DeterminerModelAlias string                  `json:"determiner_model_alias"`
	DeterminerPrompt     *string                 `json:"determiner_prompt,omitempty"`
	Posture              string                  `json:"posture"`
	Slots                []llmRoutingSlotPayload `json:"slots"`
	ContextBreakpoints   []int32                 `json:"context_breakpoints"`
	RouterInputMaxChars  int32                   `json:"router_input_max_chars"`
	DefaultSlotOnFailure int32                   `json:"default_slot_on_failure"`
}

// llmRoutingPolicyResponse mirrors the platform RoutingPolicy.
type llmRoutingPolicyResponse struct {
	ID                   string                  `json:"id"`
	OrgID                string                  `json:"org_id"`
	ModelAlias           string                  `json:"model_alias"`
	Description          *string                 `json:"description"`
	Enabled              bool                    `json:"enabled"`
	DeterminerModelAlias *string                 `json:"determiner_model_alias"`
	DeterminerPrompt     *string                 `json:"determiner_prompt"`
	Posture              string                  `json:"posture"`
	Slots                []llmRoutingSlotPayload `json:"slots"`
	ContextBreakpoints   []int32                 `json:"context_breakpoints"`
	RouterInputMaxChars  int32                   `json:"router_input_max_chars"`
	DefaultSlotOnFailure int32                   `json:"default_slot_on_failure"`
	CreatedAt            string                  `json:"created_at"`
	UpdatedAt            string                  `json:"updated_at"`
}

// llmClearableString returns the wire value of an optional string the update
// endpoint cannot null: unset is omitted on create and sent as "" on update.
func llmClearableString(v types.String, update bool) *string {
	if s, ok := knownString(v); ok {
		return &s
	}
	if update {
		empty := ""
		return &empty
	}
	return nil
}

// buildLlmRoutingPolicyRequest converts the planned model to the create
// (update=false) or update (update=true) body. Every value is known here: the
// optional settings carry schema defaults.
func buildLlmRoutingPolicyRequest(ctx context.Context, plan *llmRoutingPolicyResourceModel, update bool) (*llmRoutingPolicyRequest, diag.Diagnostics) {
	var diags diag.Diagnostics

	var slots []llmRoutingSlotModel
	diags.Append(plan.Slots.ElementsAs(ctx, &slots, false)...)
	var breakpoints []int64
	diags.Append(plan.ContextBreakpoints.ElementsAs(ctx, &breakpoints, false)...)
	if diags.HasError() {
		return nil, diags
	}

	body := &llmRoutingPolicyRequest{
		ModelAlias:           plan.ModelAlias.ValueString(),
		Description:          llmClearableString(plan.Description, update),
		Enabled:              plan.Enabled.ValueBool(),
		DeterminerModelAlias: plan.DeterminerModelAlias.ValueString(),
		DeterminerPrompt:     llmClearableString(plan.DeterminerPrompt, update),
		Posture:              plan.Posture.ValueString(),
		Slots:                make([]llmRoutingSlotPayload, 0, len(slots)),
		ContextBreakpoints:   make([]int32, 0, len(breakpoints)),
		RouterInputMaxChars:  int32(plan.RouterInputMaxChars.ValueInt64()),
		DefaultSlotOnFailure: int32(plan.DefaultSlotOnFailure.ValueInt64()),
	}
	for _, s := range slots {
		slot := llmRoutingSlotPayload{ModelAlias: s.ModelAlias.ValueString()}
		if v, ok := knownString(s.Label); ok {
			slot.Label = &v
		}
		if v, ok := knownString(s.Description); ok {
			slot.Description = &v
		}
		body.Slots = append(body.Slots, slot)
	}
	for _, b := range breakpoints {
		body.ContextBreakpoints = append(body.ContextBreakpoints, int32(b))
	}
	return body, diags
}

// applyLlmRoutingPolicyResponse maps the server's view onto a state model.
// prior is the plan (Create/Update) or previous state (Read): an empty
// description, determiner_prompt, or slot label/description settles back to
// null where prior had none, which is how a cleared value reads back.
func applyLlmRoutingPolicyResponse(ctx context.Context, policy *llmRoutingPolicyResponse, prior *llmRoutingPolicyResourceModel) (llmRoutingPolicyResourceModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	var priorSlots []llmRoutingSlotModel
	if !prior.Slots.IsNull() && !prior.Slots.IsUnknown() {
		diags.Append(prior.Slots.ElementsAs(ctx, &priorSlots, false)...)
	}
	slotValues := make([]attr.Value, 0, len(policy.Slots))
	for i, s := range policy.Slots {
		priorSlot := llmRoutingSlotModel{Label: types.StringNull(), Description: types.StringNull()}
		if i < len(priorSlots) {
			priorSlot = priorSlots[i]
		}
		obj, d := types.ObjectValue(llmRoutingSlotAttrTypes, map[string]attr.Value{
			"model_alias": types.StringValue(s.ModelAlias),
			"label":       optionalStringFromPtr(s.Label, priorSlot.Label),
			"description": optionalStringFromPtr(s.Description, priorSlot.Description),
		})
		diags.Append(d...)
		slotValues = append(slotValues, obj)
	}
	slots, d := types.ListValue(types.ObjectType{AttrTypes: llmRoutingSlotAttrTypes}, slotValues)
	diags.Append(d...)

	breakpointValues := make([]attr.Value, 0, len(policy.ContextBreakpoints))
	for _, b := range policy.ContextBreakpoints {
		breakpointValues = append(breakpointValues, types.Int64Value(int64(b)))
	}
	breakpoints, d := types.ListValue(types.Int64Type, breakpointValues)
	diags.Append(d...)

	determiner := ""
	if policy.DeterminerModelAlias != nil {
		determiner = *policy.DeterminerModelAlias
	}

	return llmRoutingPolicyResourceModel{
		ID:                   types.StringValue(policy.ID),
		OrgID:                types.StringValue(policy.OrgID),
		ModelAlias:           types.StringValue(policy.ModelAlias),
		Description:          optionalStringFromPtr(policy.Description, prior.Description),
		Enabled:              types.BoolValue(policy.Enabled),
		DeterminerModelAlias: types.StringValue(determiner),
		DeterminerPrompt:     optionalStringFromPtr(policy.DeterminerPrompt, prior.DeterminerPrompt),
		Posture:              types.StringValue(policy.Posture),
		Slots:                slots,
		ContextBreakpoints:   breakpoints,
		RouterInputMaxChars:  types.Int64Value(int64(policy.RouterInputMaxChars)),
		DefaultSlotOnFailure: types.Int64Value(int64(policy.DefaultSlotOnFailure)),
		CreatedAt:            types.StringValue(policy.CreatedAt),
		UpdatedAt:            types.StringValue(policy.UpdatedAt),
	}, diags
}
