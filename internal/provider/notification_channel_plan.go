// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"reflect"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// channelFieldRules encodes the API's polymorphic identity rules per type:
// which destination attributes are required, and which are forbidden. Sending
// a forbidden field is a 422 rather than an ignored field, so catching it at
// plan time turns a failed apply into a plan-time error.
var channelFieldRules = map[string]struct {
	required  []string
	permitted map[string]bool
}{
	channelTypeEmail: {
		required:  []string{"email_address"},
		permitted: map[string]bool{"email_address": true},
	},
	channelTypeWebhook: {
		required:  []string{"url"},
		permitted: map[string]bool{"url": true},
	},
	channelTypeSlack: {
		required:  []string{"slack_channel_id", "label"},
		permitted: map[string]bool{"slack_channel_id": true, "label": true},
	},
	channelTypeTeams: {
		// teams_workflow_url is required on CREATE only; on update it may be
		// omitted to keep the stored URL, so it is not listed as required here
		// and is checked separately below.
		required:  []string{"label"},
		permitted: map[string]bool{"label": true, "teams_workflow_url": true},
	},
}

// destinationAttrs are the type-discriminated attributes ModifyPlan polices.
var destinationAttrs = []string{"email_address", "url", "label", "slack_channel_id", "teams_workflow_url"}

func channelAttrValue(m *notificationChannelResourceModel, name string) types.String {
	switch name {
	case "email_address":
		return m.EmailAddress
	case "url":
		return m.URL
	case "label":
		return m.Label
	case "slack_channel_id":
		return m.SlackChannelID
	case "teams_workflow_url":
		return m.TeamsWorkflowURL
	}
	return types.StringNull()
}

// rotationTriggered reports whether rotate_when_changed differs between plan
// and state, i.e. the practitioner asked for a rotation.
func rotationTriggered(plan, state *notificationChannelResourceModel) bool {
	if plan.RotateWhenChanged.IsNull() && state.RotateWhenChanged.IsNull() {
		return false
	}
	if plan.RotateWhenChanged.IsUnknown() || state.RotateWhenChanged.IsUnknown() {
		return false
	}
	return !reflect.DeepEqual(plan.RotateWhenChanged.Elements(), state.RotateWhenChanged.Elements())
}

// ModifyPlan enforces the per-type destination rules and keeps signing_secret
// stable across plans that are not rotations.
func (r *notificationChannelResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Destroy plans carry a null plan; nothing to validate or preserve.
	if req.Plan.Raw.IsNull() {
		return
	}

	var plan notificationChannelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	channelType, ok := knownString(plan.Type)
	if !ok {
		return
	}

	rules, known := channelFieldRules[channelType]
	if !known {
		// The schema validator already rejects unknown and personal types; this
		// is belt-and-braces if a type is added to the validator but not here.
		resp.Diagnostics.AddAttributeError(path.Root("type"),
			"Unsupported channel type",
			fmt.Sprintf("%q has no destination-field rules in the provider. This is a bug in the provider.", channelType))
		return
	}

	for _, attr := range destinationAttrs {
		_, set := knownString(channelAttrValue(&plan, attr))
		if set && !rules.permitted[attr] {
			resp.Diagnostics.AddAttributeError(path.Root(attr),
				"Attribute not valid for this channel type",
				fmt.Sprintf("%q is not accepted for a %q channel and the API rejects it. Remove it.", attr, channelType))
		}
	}
	for _, attr := range rules.required {
		if _, set := knownString(channelAttrValue(&plan, attr)); !set {
			resp.Diagnostics.AddAttributeError(path.Root(attr),
				"Attribute required for this channel type",
				fmt.Sprintf("a %q channel requires %q.", channelType, attr))
		}
	}

	isCreate := req.State.Raw.IsNull()

	// teams_workflow_url is required on create only; an update may omit it to
	// keep the stored URL.
	if channelType == channelTypeTeams && isCreate {
		if _, set := knownString(plan.TeamsWorkflowURL); !set {
			resp.Diagnostics.AddAttributeError(path.Root("teams_workflow_url"),
				"Attribute required when creating a teams channel",
				"a \"teams\" channel requires \"teams_workflow_url\" on create; it may be omitted on update "+
					"to keep the stored URL.")
		}
	}

	if channelType != channelTypeWebhook {
		if !plan.RotateWhenChanged.IsNull() && !plan.RotateWhenChanged.IsUnknown() {
			resp.Diagnostics.AddAttributeWarning(path.Root("rotate_when_changed"),
				"rotate_when_changed has no effect on this channel type",
				fmt.Sprintf("Only \"webhook\" channels have a signing secret to rotate; this is a %q channel.", channelType))
		}
	}

	if resp.Diagnostics.HasError() || isCreate {
		return
	}

	var state notificationChannelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// signing_secret is Computed and never refreshable. Without this it would
	// plan as "(known after apply)" on every change, showing a phantom diff on
	// a sensitive value. Hold the stored value unless this plan rotates, in
	// which case it genuinely becomes unknown until apply issues the new one.
	if rotationTriggered(&plan, &state) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("signing_secret"), types.StringUnknown())...)
	} else {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("signing_secret"), state.SigningSecret)...)
	}
}
