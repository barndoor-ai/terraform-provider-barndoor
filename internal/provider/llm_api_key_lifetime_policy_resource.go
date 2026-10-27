// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmAPIKeyLifetimePath is the admin API family for the self-serve API key
// lifetime policy: the org default, and group and user overrides.
const llmAPIKeyLifetimePath = llmGatewayAPIPrefix + "/api-key-lifetime-policies"

// llmMaxLifetimeDaysLimit matches the platform's CHECK constraint on
// max_lifetime_days.
const llmMaxLifetimeDaysLimit = 3650

// llmLifetimePolicyIntro explains what every lifetime policy resource governs.
const llmLifetimePolicyIntro = "The policy limits how long API keys that users create for themselves " +
	"(self-serve, from their own settings) may live. Keys created by admins are exempt. " +
	"Existing keys keep their expiry: the policy is checked when a key is " +
	"created or rotated.\n\n" +
	"Which policy applies to a user: their own `barndoor_llm_api_key_lifetime_user_override` if they " +
	"have one; otherwise the strictest of their groups' `barndoor_llm_api_key_lifetime_group_override`s " +
	"(the shortest maximum, and an expiry required if any requires one); otherwise the organization " +
	"default (`barndoor_llm_api_key_lifetime_policy`). An override replaces what it overrides, so it can " +
	"be longer or shorter, and one with no limits set exempts its group or user."

// llmLifetimeLimitAttributes are the three limit settings every lifetime
// policy resource carries.
func llmLifetimeLimitAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"max_lifetime_days": schema.Int64Attribute{
			MarkdownDescription: fmt.Sprintf("Longest a key may live, in days from when it is created or "+
				"rotated (1-%d). A maximum also rules out keys that never expire. The platform allows a day of "+
				"slack, so \"the end of the last allowed day\" fits in any time zone. Omit for no maximum.",
				llmMaxLifetimeDaysLimit),
			Optional: true,
			Validators: []validator.Int64{
				int64validator.Between(1, llmMaxLifetimeDaysLimit),
			},
		},
		"require_expiry": schema.BoolAttribute{
			MarkdownDescription: "Keys must have an expiry, even without a maximum. Implied by " +
				"`max_lifetime_days`. Defaults to `false`.",
			Optional: true,
			Computed: true,
			Default:  booldefault.StaticBool(false),
		},
		"fixed_lifetime": schema.BoolAttribute{
			MarkdownDescription: "Every key gets exactly `max_lifetime_days`, and users don't choose an " +
				"expiry. Requires `max_lifetime_days`. Defaults to `false`.",
			Optional: true,
			Computed: true,
			Default:  booldefault.StaticBool(false),
		},
	}
}

// llmLifetimeFixedNeedsMaxValidator rejects fixed_lifetime without
// max_lifetime_days at plan time; the API answers 400.
type llmLifetimeFixedNeedsMaxValidator struct{}

func (llmLifetimeFixedNeedsMaxValidator) Description(_ context.Context) string {
	return "fixed_lifetime requires max_lifetime_days"
}

func (v llmLifetimeFixedNeedsMaxValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (llmLifetimeFixedNeedsMaxValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var fixed types.Bool
	var maxDays types.Int64
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("fixed_lifetime"), &fixed)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("max_lifetime_days"), &maxDays)...)
	if resp.Diagnostics.HasError() || maxDays.IsUnknown() {
		return
	}
	if v, ok := knownBool(fixed); ok && v && maxDays.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("fixed_lifetime"), "fixed_lifetime needs max_lifetime_days",
			"A fixed lifetime gives every key exactly max_lifetime_days, so set max_lifetime_days too.")
	}
}

// llmLifetimeLimitWire is the limit as the API reads and writes it.
type llmLifetimeLimitWire struct {
	MaxLifetimeDays *int32 `json:"max_lifetime_days"`
	RequireExpiry   bool   `json:"require_expiry"`
	FixedLifetime   bool   `json:"fixed_lifetime"`
}

// llmLifetimePolicyRow is a stored default or group override.
type llmLifetimePolicyRow struct {
	ID        string  `json:"id"`
	GroupName *string `json:"group_name"`
	llmLifetimeLimitWire
}

// llmLifetimeUserOverrideRow is a stored user override.
type llmLifetimeUserOverrideRow struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	llmLifetimeLimitWire
}

// llmLifetimePolicies is the GET (and PUT default) response. users is absent
// from platforms that predate user overrides.
type llmLifetimePolicies struct {
	Default *llmLifetimePolicyRow        `json:"default"`
	Groups  []llmLifetimePolicyRow       `json:"groups"`
	Users   []llmLifetimeUserOverrideRow `json:"users"`
}

// limitWireFrom reads the three limit attributes from a plan or state.
func limitWireFrom(ctx context.Context, src interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}, diags *diag.Diagnostics) llmLifetimeLimitWire {
	var maxDays types.Int64
	var require, fixed types.Bool
	diags.Append(src.GetAttribute(ctx, path.Root("max_lifetime_days"), &maxDays)...)
	diags.Append(src.GetAttribute(ctx, path.Root("require_expiry"), &require)...)
	diags.Append(src.GetAttribute(ctx, path.Root("fixed_lifetime"), &fixed)...)
	return llmLifetimeLimitWire{
		MaxLifetimeDays: int32PtrFromInt64(maxDays),
		RequireExpiry:   require.ValueBool(),
		FixedLifetime:   fixed.ValueBool(),
	}
}

// setLimitState writes the server's limit onto state.
func setLimitState(ctx context.Context, state *tfsdk.State, w llmLifetimeLimitWire, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("max_lifetime_days"), int64FromInt32Ptr(w.MaxLifetimeDays))...)
	diags.Append(state.SetAttribute(ctx, path.Root("require_expiry"), types.BoolValue(w.RequireExpiry))...)
	diags.Append(state.SetAttribute(ctx, path.Root("fixed_lifetime"), types.BoolValue(w.FixedLifetime))...)
}

func configureLlmClient(req resource.ConfigureRequest, resp *resource.ConfigureResponse) *client.Client {
	if req.ProviderData == nil {
		return nil
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return nil
	}
	return c
}

func requireLlmClient(c *client.Client, diags *diag.Diagnostics) bool {
	if c == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// --- barndoor_llm_api_key_lifetime_policy (org default) -------------------------

var (
	_ resource.Resource                     = &llmAPIKeyLifetimePolicyResource{}
	_ resource.ResourceWithConfigure        = &llmAPIKeyLifetimePolicyResource{}
	_ resource.ResourceWithImportState      = &llmAPIKeyLifetimePolicyResource{}
	_ resource.ResourceWithConfigValidators = &llmAPIKeyLifetimePolicyResource{}
)

// NewLlmAPIKeyLifetimePolicyResource returns a new
// barndoor_llm_api_key_lifetime_policy resource.
func NewLlmAPIKeyLifetimePolicyResource() resource.Resource {
	return &llmAPIKeyLifetimePolicyResource{}
}

// llmAPIKeyLifetimePolicyResource manages the organization's default API key
// lifetime policy, a singleton keyed by the credential's organization.
type llmAPIKeyLifetimePolicyResource struct {
	client *client.Client
}

func (r *llmAPIKeyLifetimePolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_api_key_lifetime_policy"
}

func (r *llmAPIKeyLifetimePolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attributes := llmLifetimeLimitAttributes()
	attributes["id"] = schema.StringAttribute{
		MarkdownDescription: "ID of the policy; the API keys the default by the credential's " +
			"organization, so this equals the provider's `organization_id`.",
		Computed: true,
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.UseStateForUnknown(),
		},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the organization's **default** LLM Gateway API key lifetime " +
			"policy. It is a per-organization singleton, so this resource configures it rather than " +
			"creating anything.\n\n" + llmLifetimePolicyIntro + "\n\n" +
			"With no limits set (no `max_lifetime_days`, `require_expiry = false`) the organization has no " +
			"default. `terraform destroy` clears the default the same way.",
		Attributes: attributes,
	}
}

func (r *llmAPIKeyLifetimePolicyResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{llmLifetimeFixedNeedsMaxValidator{}}
}

func (r *llmAPIKeyLifetimePolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if c := configureLlmClient(req, resp); c != nil {
		r.client = c
	}
}

func (r *llmAPIKeyLifetimePolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	r.put(ctx, &req.Plan, &resp.State, "set the default API key lifetime policy", &resp.Diagnostics)
}

func (r *llmAPIKeyLifetimePolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	r.put(ctx, &req.Plan, &resp.State, "update the default API key lifetime policy", &resp.Diagnostics)
}

func (r *llmAPIKeyLifetimePolicyResource) Read(ctx context.Context, _ resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireLlmClient(r.client, &resp.Diagnostics) {
		return
	}
	var policies llmLifetimePolicies
	if err := doJSON(ctx, r.client, http.MethodGet, llmAPIKeyLifetimePath, nil, &policies); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "API key lifetime policy",
			"read the default API key lifetime policy", err)
		return
	}
	r.setState(ctx, &resp.State, policies.Default, &resp.Diagnostics)
}

// Delete clears the default: the API removes the row when no limit is set.
func (r *llmAPIKeyLifetimePolicyResource) Delete(ctx context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !requireLlmClient(r.client, &resp.Diagnostics) {
		return
	}
	if err := doJSON(ctx, r.client, http.MethodPut, llmAPIKeyLifetimePath+"/default",
		&llmLifetimeLimitWire{}, nil); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "API key lifetime policy",
			"clear the default API key lifetime policy (destroy)", err)
	}
}

// ImportState imports the organization's default. The API resolves the
// organization from the credential, so the import ID is not looked up; pass
// the organization ID for consistency.
func (r *llmAPIKeyLifetimePolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *llmAPIKeyLifetimePolicyResource) put(ctx context.Context, plan *tfsdk.Plan, state *tfsdk.State, action string, diags *diag.Diagnostics) {
	if !requireLlmClient(r.client, diags) {
		return
	}
	body := limitWireFrom(ctx, plan, diags)
	if diags.HasError() {
		return
	}
	var policies llmLifetimePolicies
	if err := doJSON(ctx, r.client, http.MethodPut, llmAPIKeyLifetimePath+"/default", &body, &policies); err != nil {
		addLlmGatewayAPIError(diags, "API key lifetime policy", action, err)
		return
	}
	r.setState(ctx, state, policies.Default, diags)
}

// setState records the default; no row is the same as no limits.
func (r *llmAPIKeyLifetimePolicyResource) setState(ctx context.Context, state *tfsdk.State, row *llmLifetimePolicyRow, diags *diag.Diagnostics) {
	var limit llmLifetimeLimitWire
	if row != nil {
		limit = row.llmLifetimeLimitWire
	}
	diags.Append(state.SetAttribute(ctx, path.Root("id"), types.StringValue(r.client.OrganizationID()))...)
	setLimitState(ctx, state, limit, diags)
}
