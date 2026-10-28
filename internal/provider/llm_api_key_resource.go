// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// llmAPIKeyMaxGraceSecs is the platform's cap on a rotation grace period
// (7 days).
const llmAPIKeyMaxGraceSecs = 7 * 24 * 60 * 60

var llmAPIKeyUserIDFormat = stringvalidator.RegexMatches(
	regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`),
	"must be a user ID (a UUID)",
)

var (
	_ resource.Resource                     = &llmAPIKeyResource{}
	_ resource.ResourceWithConfigure        = &llmAPIKeyResource{}
	_ resource.ResourceWithImportState      = &llmAPIKeyResource{}
	_ resource.ResourceWithConfigValidators = &llmAPIKeyResource{}
)

// NewLlmAPIKeyResource returns a new barndoor_llm_api_key resource.
func NewLlmAPIKeyResource() resource.Resource {
	return &llmAPIKeyResource{}
}

// llmAPIKeyResource manages an LLM Gateway API key through the admin REST API
// (`/api/llm-gateway/admin/api-keys`).
type llmAPIKeyResource struct {
	client *client.Client
}

type llmAPIKeyResourceModel struct {
	ID                   types.String `tfsdk:"id"`
	Name                 types.String `tfsdk:"name"`
	UserID               types.String `tfsdk:"user_id"`
	GroupName            types.String `tfsdk:"group_name"`
	Scopes               types.List   `tfsdk:"scopes"`
	ExpiresAt            types.String `tfsdk:"expires_at"`
	Disabled             types.Bool   `tfsdk:"disabled"`
	RotationTrigger      types.Map    `tfsdk:"rotation_trigger"`
	GracePeriodSecs      types.Int64  `tfsdk:"grace_period_secs"`
	Key                  types.String `tfsdk:"key"`
	KeyPrefix            types.String `tfsdk:"key_prefix"`
	Kind                 types.String `tfsdk:"kind"`
	CreatedAt            types.String `tfsdk:"created_at"`
	RotatedAt            types.String `tfsdk:"rotated_at"`
	PreviousKeyExpiresAt types.String `tfsdk:"previous_key_expires_at"`
}

func (r *llmAPIKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_api_key"
}

func (r *llmAPIKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	// Values that only change when the secret does: kept from state unless
	// rotation_trigger changes.
	secretBound := func(doc string, sensitive bool) schema.StringAttribute {
		return schema.StringAttribute{
			MarkdownDescription: doc,
			Computed:            true,
			Sensitive:           sensitive,
			PlanModifiers:       []planmodifier.String{llmAPIKeyKeepUnlessRotating{}},
		}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an LLM Gateway API key: the credential apps and agents use to call the " +
			"gateway.\n\n" +
			"~> **The key's secret is stored in Terraform state.** The platform returns it only when the key " +
			"is created or rotated, so `key` is recorded in state (marked sensitive) for you to pass on, for " +
			"example to a secrets manager. Protect your state accordingly: use an encrypted remote backend " +
			"with restricted access.\n\n" +
			"Rotate the secret in place by changing `rotation_trigger`: the key keeps its id, agent binding and " +
			"usage history, and with `grace_period_secs` the previous secret keeps working for that long so " +
			"clients can move over. `expires_at` and `disabled` also change in place. `name`, `user_id`, " +
			"`group_name` and `scopes` can't change on an existing key, so changing them creates a new key with " +
			"a new secret. Destroying the resource revokes the key.\n\n" +
			"Keys created here are admin keys, exempt from the self-serve API key lifetime policy.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Key UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Key name. Unique among the live keys of its user, its group, or the " +
					"organization (409 otherwise). Changing it creates a new key.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"user_id": schema.StringAttribute{
				MarkdownDescription: "Assign the key to a user. Their identity, groups and policies apply to " +
					"its requests. Conflicts with `group_name`; set neither for a service key. Changing it " +
					"creates a new key.",
				Optional:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{llmAPIKeyUserIDFormat},
			},
			"group_name": schema.StringAttribute{
				MarkdownDescription: "Bind the key to an IdP group, so group-scoped budgets, rate limits and " +
					"model access apply to it. Conflicts with `user_id`. Changing it creates a new key.",
				Optional:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`\S`), "must not be blank")},
			},
			"scopes": schema.ListAttribute{
				MarkdownDescription: "Scopes recorded on the key. Defaults to none. Changing them creates a " +
					"new key.",
				ElementType:   types.StringType,
				Optional:      true,
				Computed:      true,
				Default:       listdefault.StaticValue(types.ListValueMust(types.StringType, []attr.Value{})),
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
				Validators:    []validator.List{listvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1))},
			},
			"expires_at": schema.StringAttribute{
				MarkdownDescription: "When the key stops working, as an RFC 3339 timestamp (e.g. " +
					"`2027-01-01T00:00:00Z`). It must be in the future when it is set. Changing or removing it " +
					"updates the key in place and keeps its secret; extending an expired key reactivates it. " +
					"Omit for a key that never expires. Use a fixed timestamp: one computed from `timestamp()` " +
					"changes on every plan.",
				Optional:   true,
				Validators: []validator.String{llmAPIKeyRFC3339Validator{}},
			},
			"disabled": schema.BoolAttribute{
				MarkdownDescription: "Temporarily stop the key from working, without revoking it. Defaults to " +
					"`false`.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
			},
			"rotation_trigger": schema.MapAttribute{
				MarkdownDescription: "Arbitrary values whose change rotates the key's secret in place, for " +
					"example `{ rotated = time_rotating.monthly.id }`. Setting it for the first time on an " +
					"existing key also rotates it; removing it doesn't. A rotation that also changes " +
					"`expires_at` applies both together.",
				ElementType: types.StringType,
				Optional:    true,
			},
			"grace_period_secs": schema.Int64Attribute{
				MarkdownDescription: fmt.Sprintf("How long the previous secret keeps working after a "+
					"rotation, in seconds (0-%d, 7 days). Defaults to `0`: the old secret stops immediately. "+
					"Changing it alone doesn't rotate; it applies to the next rotation.", llmAPIKeyMaxGraceSecs),
				Optional:   true,
				Computed:   true,
				Default:    int64default.StaticInt64(0),
				Validators: []validator.Int64{int64validator.Between(0, llmAPIKeyMaxGraceSecs)},
			},
			"key": secretBound("The key's secret. Sensitive, and stored in state. Changes when the key "+
				"rotates. Null after `terraform import`, because the platform never returns it again: change "+
				"`rotation_trigger` to issue one Terraform can see.", true),
			"key_prefix": secretBound("The first characters of the secret, shown in the Barndoor app to "+
				"identify the key. Not sensitive.", false),
			"rotated_at":              secretBound("When the secret was last rotated; null if never.", false),
			"previous_key_expires_at": secretBound("When the previous secret stops working after the last rotation's grace period; null if none.", false),
			"kind": schema.StringAttribute{
				MarkdownDescription: "`user` for a key assigned to a user, `machine` otherwise.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the key was created.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *llmAPIKeyResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.Conflicting(path.MatchRoot("user_id"), path.MatchRoot("group_name")),
	}
}

func (r *llmAPIKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *llmAPIKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}
	var plan llmAPIKeyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := llmAPIKeyCreateRequest{Name: plan.Name.ValueString(), Scopes: []string{}}
	if v, ok := knownString(plan.UserID); ok {
		body.UserID = &v
	}
	if v, ok := knownString(plan.GroupName); ok {
		body.GroupName = &v
	}
	if v, ok := knownString(plan.ExpiresAt); ok {
		body.ExpiresAt = &v
	}
	resp.Diagnostics.Append(plan.Scopes.ElementsAs(ctx, &body.Scopes, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var created llmAPIKeySecretResponse
	if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/api-keys", &body, &created); err != nil {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM API key", "create the LLM API key", err)
		return
	}
	if created.ID == "" || created.Key == "" {
		resp.Diagnostics.AddError("Malformed LLM Gateway API response", "Create returned no key id or secret.")
		return
	}

	// Record the key and its secret before any follow-up call: the secret is
	// never returned again, and a failure below must not orphan the key.
	state := plan
	state.ID = types.StringValue(created.ID)
	state.Key = types.StringValue(created.Key)
	state.KeyPrefix = types.StringValue(created.KeyPrefix)
	state.RotatedAt = types.StringNull()
	state.PreviousKeyExpiresAt = types.StringNull()
	state.Kind = types.StringNull()
	state.CreatedAt = types.StringValue(created.CreatedAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The create endpoint has no `disabled` field: new keys are enabled.
	if plan.Disabled.ValueBool() {
		if !r.setDisabled(ctx, created.ID, true, &resp.Diagnostics) {
			return
		}
	}
	r.refresh(ctx, &state, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *llmAPIKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}
	var state llmAPIKeyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if gone := r.refresh(ctx, &state, &resp.Diagnostics); gone {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *llmAPIKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}
	var plan, prior llmAPIKeyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := prior.ID.ValueString()

	// Carry the secret-bound values forward; a rotation replaces them below.
	state := plan
	state.ID = prior.ID
	state.Key = prior.Key
	state.KeyPrefix = prior.KeyPrefix
	state.RotatedAt = prior.RotatedAt
	state.PreviousKeyExpiresAt = prior.PreviousKeyExpiresAt
	state.Kind = prior.Kind
	state.CreatedAt = prior.CreatedAt

	expiryChanged := !sameInstant(plan.ExpiresAt, prior.ExpiresAt)
	if llmAPIKeyRotates(plan.RotationTrigger, prior.RotationTrigger) {
		body := map[string]any{"grace_period_secs": plan.GracePeriodSecs.ValueInt64()}
		if expiryChanged {
			body["expires_at"] = stringPtrOrNil(plan.ExpiresAt) // null clears
		}
		var rotated llmAPIKeySecretResponse
		if err := doJSON(ctx, r.client, http.MethodPost, llmGatewayAPIPrefix+"/api-keys/"+id+"/rotate", body, &rotated); err != nil {
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM API key", "rotate the LLM API key", err)
			return
		}
		state.Key = types.StringValue(rotated.Key)
		state.KeyPrefix = types.StringValue(rotated.KeyPrefix)
		// Save the new secret before anything else can fail: the old one may
		// already have stopped working.
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	} else if expiryChanged {
		body := map[string]any{"expires_at": stringPtrOrNil(plan.ExpiresAt)}
		if err := doJSON(ctx, r.client, http.MethodPut, llmGatewayAPIPrefix+"/api-keys/"+id+"/expiry", body, nil); err != nil {
			addLlmGatewayAPIError(&resp.Diagnostics, "LLM API key", "change the LLM API key's expiry", err)
			return
		}
	}

	if !plan.Disabled.Equal(prior.Disabled) {
		if !r.setDisabled(ctx, id, plan.Disabled.ValueBool(), &resp.Diagnostics) {
			return
		}
	}

	r.refresh(ctx, &state, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete revokes the key. A 404 means it is unknown or already revoked, which
// is success for a destroy.
func (r *llmAPIKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}
	var state llmAPIKeyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := doJSON(ctx, r.client, http.MethodDelete, llmGatewayAPIPrefix+"/api-keys/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addLlmGatewayAPIError(&resp.Diagnostics, "LLM API key", "revoke the LLM API key", err)
	}
}

// ImportState imports a key by id. Its secret can't be imported: the platform
// never returns it again, so `key` stays null until the next rotation.
func (r *llmAPIKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("disabled"), types.BoolValue(false))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("grace_period_secs"), types.Int64Value(0))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("rotation_trigger"), types.MapNull(types.StringType))...)
}

// refresh reads the key and maps the server's view onto m. It reports true
// when the key is gone (unknown or revoked); revocation is terminal, so a
// revoked key is treated as deleted and the next plan creates a new one.
func (r *llmAPIKeyResource) refresh(ctx context.Context, m *llmAPIKeyResourceModel, diags *diag.Diagnostics) bool {
	var key llmAPIKeyResponse
	err := doJSON(ctx, r.client, http.MethodGet, llmGatewayAPIPrefix+"/api-keys/"+m.ID.ValueString(), nil, &key)
	if err != nil {
		if isNotFound(err) {
			return true
		}
		addLlmGatewayAPIError(diags, "LLM API key", "read the LLM API key", err)
		return false
	}
	if key.RevokedAt != nil {
		return true
	}

	m.Name = types.StringValue(key.Name)
	m.UserID = keepSpelling(key.UserID, m.UserID, strings.EqualFold)
	m.GroupName = keepSpelling(key.GroupName, m.GroupName, func(prior, server string) bool {
		return strings.TrimSpace(prior) == server
	})
	scopes := make([]attr.Value, 0, len(key.Scopes))
	for _, s := range key.Scopes {
		scopes = append(scopes, types.StringValue(s))
	}
	m.Scopes = types.ListValueMust(types.StringType, scopes)
	m.ExpiresAt = settleInstant(key.ExpiresAt, m.ExpiresAt)
	m.Disabled = types.BoolValue(key.DisabledAt != nil)
	m.KeyPrefix = types.StringValue(key.KeyPrefix)
	m.Kind = types.StringValue(key.Kind)
	m.CreatedAt = settleInstant(&key.CreatedAt, m.CreatedAt)
	m.RotatedAt = settleInstant(key.RotatedAt, m.RotatedAt)
	m.PreviousKeyExpiresAt = settleInstant(key.PreviousKeyExpiresAt, m.PreviousKeyExpiresAt)
	if m.Key.IsUnknown() {
		m.Key = types.StringNull()
	}
	return false
}

func (r *llmAPIKeyResource) setDisabled(ctx context.Context, id string, disabled bool, diags *diag.Diagnostics) bool {
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	if err := doJSON(ctx, r.client, http.MethodPatch, llmGatewayAPIPrefix+"/api-keys/"+id,
		map[string]bool{"disabled": disabled}, nil); err != nil {
		addLlmGatewayAPIError(diags, "LLM API key", verb+" the LLM API key", err)
		return false
	}
	return true
}

func (r *llmAPIKeyResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// llmAPIKeyKeepUnlessRotating keeps a secret-bound computed value from state,
// and leaves it unknown when rotation_trigger changes (or on create).
type llmAPIKeyKeepUnlessRotating struct{}

func (llmAPIKeyKeepUnlessRotating) Description(_ context.Context) string {
	return "Keeps the prior value unless rotation_trigger changes."
}

func (m llmAPIKeyKeepUnlessRotating) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (llmAPIKeyKeepUnlessRotating) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || !req.PlanValue.IsUnknown() {
		return
	}
	var planned, prior types.Map
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("rotation_trigger"), &planned)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("rotation_trigger"), &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// An unknown trigger may turn out to rotate, so the secret stays unknown.
	if !planned.IsUnknown() && !llmAPIKeyRotates(planned, prior) {
		resp.PlanValue = req.StateValue
	}
}

// llmAPIKeyRotates reports whether a plan rotates the key: rotation_trigger
// is set and differs from state. Removing it does not rotate.
func llmAPIKeyRotates(planned, prior types.Map) bool {
	return !planned.IsNull() && !planned.IsUnknown() && !planned.Equal(prior)
}

// llmAPIKeyRFC3339Validator checks expires_at parses as RFC 3339.
type llmAPIKeyRFC3339Validator struct{}

func (llmAPIKeyRFC3339Validator) Description(_ context.Context) string {
	return "value must be an RFC 3339 timestamp"
}

func (v llmAPIKeyRFC3339Validator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (llmAPIKeyRFC3339Validator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := time.Parse(time.RFC3339Nano, req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid RFC 3339 timestamp",
			fmt.Sprintf("expires_at must be an RFC 3339 timestamp (e.g. 2027-01-01T00:00:00Z): %s", err))
	}
}

// sameInstant reports whether two optional RFC 3339 values name the same
// instant (both null counts as the same).
func sameInstant(a, b types.String) bool {
	av, aok := knownString(a)
	bv, bok := knownString(b)
	if !aok || !bok {
		return aok == bok
	}
	at, aerr := time.Parse(time.RFC3339Nano, av)
	bt, berr := time.Parse(time.RFC3339Nano, bv)
	if aerr != nil || berr != nil {
		return av == bv
	}
	return at.Equal(bt)
}

// settleInstant records the server's timestamp, keeping the prior spelling
// when it names the same instant (the platform may answer with a different
// offset or precision than was configured).
func settleInstant(server *string, prior types.String) types.String {
	if server == nil {
		return types.StringNull()
	}
	s := types.StringValue(*server)
	if sameInstant(s, prior) {
		return prior
	}
	return s
}

// keepSpelling records the server's value, keeping the prior spelling when
// same reports it names the same thing.
func keepSpelling(server *string, prior types.String, same func(prior, server string) bool) types.String {
	if server == nil {
		return types.StringNull()
	}
	if p, ok := knownString(prior); ok && same(p, *server) {
		return prior
	}
	return types.StringValue(*server)
}

func stringPtrOrNil(v types.String) *string {
	if s, ok := knownString(v); ok {
		return &s
	}
	return nil
}

// --- request/response DTOs ---------------------------------------------------

type llmAPIKeyCreateRequest struct {
	Name      string   `json:"name"`
	UserID    *string  `json:"user_id,omitempty"`
	GroupName *string  `json:"group_name,omitempty"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at,omitempty"`
}

// llmAPIKeySecretResponse is the create and rotate response: the only two
// that carry the secret.
type llmAPIKeySecretResponse struct {
	ID        string `json:"id"`
	KeyPrefix string `json:"key_prefix"`
	Key       string `json:"key"`
	CreatedAt string `json:"created_at"`
}

// llmAPIKeyResponse mirrors the gateway's ApiKey.
type llmAPIKeyResponse struct {
	ID                   string   `json:"id"`
	Kind                 string   `json:"kind"`
	UserID               *string  `json:"user_id"`
	Name                 string   `json:"name"`
	KeyPrefix            string   `json:"key_prefix"`
	Scopes               []string `json:"scopes"`
	GroupName            *string  `json:"group_name"`
	ExpiresAt            *string  `json:"expires_at"`
	CreatedAt            string   `json:"created_at"`
	RevokedAt            *string  `json:"revoked_at"`
	DisabledAt           *string  `json:"disabled_at"`
	RotatedAt            *string  `json:"rotated_at"`
	PreviousKeyExpiresAt *string  `json:"previous_key_expires_at"`
}
