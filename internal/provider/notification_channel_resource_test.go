// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func mustStringSet(t *testing.T, vals ...string) types.Set {
	t.Helper()
	s, diags := types.SetValueFrom(context.Background(), types.StringType, vals)
	if diags.HasError() {
		t.Fatalf("SetValueFrom: %v", diags)
	}
	return s
}

func mustStringMap(t *testing.T, kv map[string]string) types.Map {
	t.Helper()
	m, diags := types.MapValueFrom(context.Background(), types.StringType, kv)
	if diags.HasError() {
		t.Fatalf("MapValueFrom: %v", diags)
	}
	return m
}

func webhookResponse() *notificationChannelResponse {
	url := "https://hooks.example.com/barndoor"
	return &notificationChannelResponse{
		ID:               "chan-1",
		Type:             channelTypeWebhook,
		Enabled:          true,
		URL:              &url,
		Subscriptions:    []notificationSubscriptionBody{{AlertType: "break_glass_used"}},
		CreatedAt:        "2026-08-28T00:00:00Z",
		UpdatedAt:        "2026-08-28T00:00:00Z",
		HasSigningSecret: true,
	}
}

// The single most important behaviour in this resource. The platform reveals
// the signing secret once and can never return it, so a read MUST carry the
// prior value through. Nulling it would show a phantom diff on a sensitive
// attribute on every plan, forever.
func TestApplyChannelResponse_ReadPreservesSigningSecret(t *testing.T) {
	prior := &notificationChannelResourceModel{
		SigningSecret: types.StringValue("whsec_original"),
		Subscriptions: mustStringSet(t, "break_glass_used"),
	}

	// revealed == nil models a read: the API returned no secret.
	state := applyChannelResponse(webhookResponse(), prior, nil)

	if state.SigningSecret.IsNull() || state.SigningSecret.IsUnknown() {
		t.Fatal("signing_secret must be preserved on read, not nulled — nulling causes a permanent diff")
	}
	if got := state.SigningSecret.ValueString(); got != "whsec_original" {
		t.Errorf("signing_secret = %q, want the prior value whsec_original", got)
	}
	if !state.HasSigningSecret.ValueBool() {
		t.Error("has_signing_secret must be refreshed from the API")
	}
}

func TestApplyChannelResponse_RevealOverwritesSigningSecret(t *testing.T) {
	prior := &notificationChannelResourceModel{
		SigningSecret: types.StringValue("whsec_original"),
		Subscriptions: mustStringSet(t, "break_glass_used"),
	}
	rotated := "whsec_rotated"

	state := applyChannelResponse(webhookResponse(), prior, &rotated)

	if got := state.SigningSecret.ValueString(); got != rotated {
		t.Errorf("signing_secret = %q, want the freshly revealed %q", got, rotated)
	}
}

// An empty reveal must not clobber a stored secret. The API omits the field on
// every non-reveal response, and a defensive empty string must be treated the
// same as absent.
func TestApplyChannelResponse_EmptyRevealDoesNotClobber(t *testing.T) {
	prior := &notificationChannelResourceModel{
		SigningSecret: types.StringValue("whsec_original"),
		Subscriptions: mustStringSet(t, "break_glass_used"),
	}
	empty := ""

	state := applyChannelResponse(webhookResponse(), prior, &empty)

	if got := state.SigningSecret.ValueString(); got != "whsec_original" {
		t.Errorf("signing_secret = %q, want the stored value retained", got)
	}
}

// teams_workflow_url is write-only: the API never returns it, so state must
// follow configuration rather than being nulled from an absent response field.
func TestApplyChannelResponse_TeamsWorkflowURLFollowsConfig(t *testing.T) {
	label := "Ops"
	resp := &notificationChannelResponse{
		ID: "chan-2", Type: channelTypeTeams, Enabled: true,
		Label: &label, HasWorkflowURL: true,
	}
	prior := &notificationChannelResourceModel{
		TeamsWorkflowURL: types.StringValue("https://example.logic.azure.com/workflows/abc"),
	}

	state := applyChannelResponse(resp, prior, nil)

	if state.TeamsWorkflowURL.ValueString() != "https://example.logic.azure.com/workflows/abc" {
		t.Error("teams_workflow_url must follow configuration; the API never returns it")
	}
	if !state.HasWorkflowURL.ValueBool() {
		t.Error("has_workflow_url is the observable signal and must come from the API")
	}
}

// An omitted config set and an empty server set both mean "delivers nothing";
// echoing an empty set where the config said nothing would be perpetual drift.
func TestApplyChannelResponse_NullSubscriptionsStayNull(t *testing.T) {
	resp := webhookResponse()
	resp.Subscriptions = nil
	prior := &notificationChannelResourceModel{Subscriptions: types.SetNull(types.StringType)}

	state := applyChannelResponse(resp, prior, nil)

	if !state.Subscriptions.IsNull() {
		t.Error("an unset subscriptions config must stay null, not become an empty set")
	}
}

func TestBuildChannelWriteRequest_OnlyPermittedDestinationFields(t *testing.T) {
	plan := &notificationChannelResourceModel{
		Type:          types.StringValue(channelTypeEmail),
		Enabled:       types.BoolValue(true),
		EmailAddress:  types.StringValue("ops@example.com"),
		URL:           types.StringNull(),
		Label:         types.StringNull(),
		Subscriptions: mustStringSet(t, "connection_broken"),
	}

	raw, err := json.Marshal(buildChannelWriteRequest(plan, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Sending a field that does not belong to the type is a 422 server-side, so
	// the body must not be padded with nulls for the other types' fields.
	for _, forbidden := range []string{"url", "label", "slack_channel_id", "teams_workflow_url", "id"} {
		if _, present := body[forbidden]; present {
			t.Errorf("%q must not appear in an email channel body", forbidden)
		}
	}
	if body["email_address"] != "ops@example.com" {
		t.Errorf("email_address missing: %#v", body["email_address"])
	}
}

// The endpoint REPLACES the subscription set, so an absent key and an empty
// list mean the same destructive thing. Send it explicitly rather than relying
// on server-side defaulting.
func TestBuildChannelWriteRequest_AlwaysSendsSubscriptions(t *testing.T) {
	plan := &notificationChannelResourceModel{
		Type:          types.StringValue(channelTypeWebhook),
		Enabled:       types.BoolValue(true),
		URL:           types.StringValue("https://hooks.example.com/barndoor"),
		Subscriptions: types.SetNull(types.StringType),
	}

	raw, _ := json.Marshal(buildChannelWriteRequest(plan, false))
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	subs, present := body["subscriptions"]
	if !present {
		t.Fatal("subscriptions must always be sent, even when unset")
	}
	if list, ok := subs.([]any); !ok || len(list) != 0 {
		t.Errorf("want an empty list, got %#v", subs)
	}
}

func TestBuildChannelWriteRequest_IncludesIDOnUpdateOnly(t *testing.T) {
	plan := &notificationChannelResourceModel{
		ID:            types.StringValue("chan-1"),
		Type:          types.StringValue(channelTypeWebhook),
		Enabled:       types.BoolValue(true),
		URL:           types.StringValue("https://hooks.example.com/barndoor"),
		Subscriptions: types.SetNull(types.StringType),
	}

	if got := buildChannelWriteRequest(plan, false); got.ID != nil {
		t.Error("create must not send an id")
	}
	got := buildChannelWriteRequest(plan, true)
	if got.ID == nil || *got.ID != "chan-1" {
		t.Errorf("update must send the tracked id, got %#v", got.ID)
	}
}

func TestRotationTriggered(t *testing.T) {
	tests := []struct {
		name        string
		plan, state types.Map
		want        bool
	}{
		{"both null", types.MapNull(types.StringType), types.MapNull(types.StringType), false},
		{"unchanged", mustStringMap(t, map[string]string{"ts": "1"}), mustStringMap(t, map[string]string{"ts": "1"}), false},
		{"value changed", mustStringMap(t, map[string]string{"ts": "2"}), mustStringMap(t, map[string]string{"ts": "1"}), true},
		{"key added", mustStringMap(t, map[string]string{"ts": "1", "x": "y"}), mustStringMap(t, map[string]string{"ts": "1"}), true},
		{"newly set", mustStringMap(t, map[string]string{"ts": "1"}), types.MapNull(types.StringType), true},
		{"removed", types.MapNull(types.StringType), mustStringMap(t, map[string]string{"ts": "1"}), true},
		{"unknown is not a rotation", types.MapUnknown(types.StringType), mustStringMap(t, map[string]string{"ts": "1"}), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rotationTriggered(
				&notificationChannelResourceModel{RotateWhenChanged: tc.plan},
				&notificationChannelResourceModel{RotateWhenChanged: tc.state},
			)
			if got != tc.want {
				t.Errorf("rotationTriggered = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every org-wide type must have field rules, or ModifyPlan would reject a type
// the schema validator accepts.
func TestChannelFieldRules_CoverEveryValidatedType(t *testing.T) {
	for _, ct := range []string{channelTypeEmail, channelTypeWebhook, channelTypeSlack, channelTypeTeams} {
		if _, ok := channelFieldRules[ct]; !ok {
			t.Errorf("no destination-field rules for %q, which the schema validator accepts", ct)
		}
	}
	// The personal types must NOT be manageable: their owner derives from the
	// caller's token, so a Terraform credential could only manage its own.
	for _, ct := range []string{channelTypeInApp, channelTypeUserEmail} {
		if _, ok := channelFieldRules[ct]; ok {
			t.Errorf("%q is a personal channel type and must not be manageable", ct)
		}
	}
}

// Every attribute ModifyPlan polices must be readable, or a forbidden field
// would slip through as a silent null.
func TestChannelAttrValue_CoversEveryPolicedAttribute(t *testing.T) {
	m := &notificationChannelResourceModel{
		EmailAddress:     types.StringValue("a"),
		URL:              types.StringValue("b"),
		Label:            types.StringValue("c"),
		SlackChannelID:   types.StringValue("d"),
		TeamsWorkflowURL: types.StringValue("e"),
	}
	for _, attr := range destinationAttrs {
		if v := channelAttrValue(m, attr); v.IsNull() {
			t.Errorf("channelAttrValue(%q) returned null; ModifyPlan cannot police it", attr)
		}
	}
}
