// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// This file holds what barndoor_dlp_mcp_policy and
// barndoor_dlp_mcp_baseline_policy share: the `rules` schema, its wire shape,
// the state mapping, rule-id reuse across saves, and error diagnostics for the
// dlp-service `/mcp-policies` and `/mcp-baseline-policy` routes.

// MCP policy rule enum wire values and API limits.
var (
	dlpMcpRuntimeBoundaries = []string{"TOOL_INPUT", "TOOL_OUTPUT", "BOTH"}
	dlpMcpScopeModes        = []string{dlpMcpScopeAll, dlpMcpScopeOnly, dlpMcpScopeExcept}
	dlpMcpMaskKeeps         = []string{"PREFIX", "SUFFIX"}
)

const (
	dlpMcpScopeAll    = "ALL"
	dlpMcpScopeOnly   = "ONLY"
	dlpMcpScopeExcept = "EXCEPT"

	dlpMcpMaxRules        = 50
	dlpMcpMaxLabelChars   = 120
	dlpMcpMaxScopeEntries = 200
	dlpMcpMaxKeepChars    = 16

	dlpMcpDefaultMaskChar = "X"
	dlpMcpMaskAction      = "POLICY_ACTION_MASK"

	// dlpMcpServerAlreadyAssignedCode is the stable `code` of the API's 409
	// for a save that would put an MCP server in a second policy.
	dlpMcpServerAlreadyAssignedCode = "mcp_server_already_assigned"

	// dlpMcpNotConvertedFragment identifies the API's 409 for an
	// organization that still manages MCP traffic through enforcement
	// policies, where the MCP-policy routes are read-only.
	dlpMcpNotConvertedFragment = "doesn't use MCP policies yet"
)

// dlpMcpLowerUUID matches the canonical lowercase UUID form the API echoes
// back for agent and detection engine ids. Any other spelling of the same id
// would read back differently and fail the apply with an inconsistent-result
// error, so it is rejected at plan time.
var dlpMcpLowerUUID = stringvalidator.RegexMatches(
	regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`),
	"must be a lowercase UUID",
)

// --- models -----------------------------------------------------------------------

// dlpMcpRuleModel maps one entry of the rules list. The nested objects are
// pointers so an omitted block round-trips as nil.
type dlpMcpRuleModel struct {
	Label              types.String               `tfsdk:"label"`
	RuntimeBoundary    types.String               `tfsdk:"runtime_boundary"`
	Action             types.String               `tfsdk:"action"`
	DetectionEngineIDs types.List                 `tfsdk:"detection_engine_ids"`
	DryRun             types.Bool                 `tfsdk:"dry_run"`
	MaskFormat         *dlpMcpMaskFormatModel     `tfsdk:"mask_format"`
	AgentScope         *dlpMcpAgentScopeModel     `tfsdk:"agent_scope"`
	PrincipalScope     *dlpMcpPrincipalScopeModel `tfsdk:"principal_scope"`
}

type dlpMcpMaskFormatModel struct {
	Keep               types.String `tfsdk:"keep"`
	KeepChars          types.Int64  `tfsdk:"keep_chars"`
	MaskChar           types.String `tfsdk:"mask_char"`
	PreserveSeparators types.Bool   `tfsdk:"preserve_separators"`
}

type dlpMcpAgentScopeModel struct {
	Mode     types.String `tfsdk:"mode"`
	AgentIDs types.Set    `tfsdk:"agent_ids"`
}

// dlpMcpPrincipalScopeModel's Principals is a plain slice so an omitted set
// round-trips as nil.
type dlpMcpPrincipalScopeModel struct {
	Mode       types.String        `tfsdk:"mode"`
	Principals []dlpPrincipalModel `tfsdk:"principals"`
}

// --- schema -----------------------------------------------------------------------

// dlpMcpRulesAttribute is the `rules` attribute both MCP policy resources
// carry.
func dlpMcpRulesAttribute(description string) schema.ListNestedAttribute {
	return schema.ListNestedAttribute{
		MarkdownDescription: description,
		Required:            true,
		Validators: []validator.List{
			listvalidator.SizeAtMost(dlpMcpMaxRules),
		},
		NestedObject: schema.NestedAttributeObject{
			Attributes: map[string]schema.Attribute{
				"label": schema.StringAttribute{
					MarkdownDescription: fmt.Sprintf("Optional display label for the rule, up to %d characters.",
						dlpMcpMaxLabelChars),
					Optional: true,
					Validators: []validator.String{
						stringvalidator.LengthBetween(1, dlpMcpMaxLabelChars),
					},
				},
				"runtime_boundary": schema.StringAttribute{
					MarkdownDescription: "Which tool traffic the rule inspects: `TOOL_INPUT` (arguments sent to " +
						"the tool), `TOOL_OUTPUT` (what the tool returns), or `BOTH`.",
					Required: true,
					Validators: []validator.String{
						stringvalidator.OneOf(dlpMcpRuntimeBoundaries...),
					},
				},
				"action": schema.StringAttribute{
					MarkdownDescription: "Action taken when a detection engine reports a finding: " +
						"`POLICY_ACTION_TOKENIZE`, `POLICY_ACTION_REDACT`, `POLICY_ACTION_PASSTHROUGH`, " +
						"`POLICY_ACTION_ALERT_ONLY`, `POLICY_ACTION_BLOCK`, `POLICY_ACTION_MASK`, or " +
						"`POLICY_ACTION_OMIT`. Every referenced detection engine must support the action.",
					Required: true,
					Validators: []validator.String{
						stringvalidator.OneOf(dlpPolicyActions...),
					},
				},
				"detection_engine_ids": schema.ListAttribute{
					MarkdownDescription: "IDs (lowercase UUIDs) of the detection engines that trigger the " +
						"rule. At least one is required, and each must belong to the organization.",
					ElementType: types.StringType,
					Required:    true,
					Validators: []validator.List{
						listvalidator.SizeAtLeast(1),
						listvalidator.UniqueValues(),
						listvalidator.ValueStringsAre(dlpMcpLowerUUID),
					},
				},
				"dry_run": schema.BoolAttribute{
					MarkdownDescription: "Whether the rule only observes (records findings without acting). " +
						"Defaults to `false`.",
					Optional: true,
					Computed: true,
					Default:  booldefault.StaticBool(false),
				},
				"mask_format": schema.SingleNestedAttribute{
					MarkdownDescription: "Partial-redaction shape: keep a prefix or suffix of the matched value " +
						"readable and mask the rest. Only valid with `action = \"POLICY_ACTION_MASK\"`; omit it " +
						"to mask the whole value.",
					Optional: true,
					Attributes: map[string]schema.Attribute{
						"keep": schema.StringAttribute{
							MarkdownDescription: "Which end of the value stays readable: `PREFIX` or `SUFFIX`.",
							Required:            true,
							Validators: []validator.String{
								stringvalidator.OneOf(dlpMcpMaskKeeps...),
							},
						},
						"keep_chars": schema.Int64Attribute{
							MarkdownDescription: fmt.Sprintf("How many characters stay readable, 0 to %d.",
								dlpMcpMaxKeepChars),
							Required: true,
							Validators: []validator.Int64{
								int64validator.Between(0, dlpMcpMaxKeepChars),
							},
						},
						"mask_char": schema.StringAttribute{
							MarkdownDescription: "Character that replaces masked characters. Defaults to `X`.",
							Optional:            true,
							Computed:            true,
							Default:             stringdefault.StaticString(dlpMcpDefaultMaskChar),
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
							},
						},
						"preserve_separators": schema.BoolAttribute{
							MarkdownDescription: "Whether separators such as `-` stay in place. Defaults to `true`.",
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(true),
						},
					},
				},
				"agent_scope": schema.SingleNestedAttribute{
					MarkdownDescription: "AI agents the rule applies to. Omit to apply to every agent, " +
						"including agents created later.",
					Optional: true,
					Attributes: map[string]schema.Attribute{
						"mode": schema.StringAttribute{
							MarkdownDescription: "`ALL` (every agent), `ONLY` (just the agents in `agent_ids`), " +
								"or `EXCEPT` (every agent but those in `agent_ids`).",
							Required: true,
							Validators: []validator.String{
								stringvalidator.OneOf(dlpMcpScopeModes...),
							},
						},
						"agent_ids": schema.SetAttribute{
							MarkdownDescription: fmt.Sprintf("IDs (lowercase UUIDs) of registry agents: 1 to %d, "+
								"required for `ONLY` and `EXCEPT` and not allowed for `ALL`.", dlpMcpMaxScopeEntries),
							ElementType: types.StringType,
							Optional:    true,
							Validators: []validator.Set{
								setvalidator.SizeBetween(1, dlpMcpMaxScopeEntries),
								setvalidator.ValueStringsAre(dlpMcpLowerUUID),
							},
						},
					},
				},
				"principal_scope": schema.SingleNestedAttribute{
					MarkdownDescription: "Users the rule applies to, by IdP group or role. Omit to apply to " +
						"every user. A user matches the list when any of their groups or roles is in it.",
					Optional: true,
					Attributes: map[string]schema.Attribute{
						"mode": schema.StringAttribute{
							MarkdownDescription: "`ALL` (every user), `ONLY` (just the listed groups and roles), " +
								"or `EXCEPT` (every user but the listed groups and roles).",
							Required: true,
							Validators: []validator.String{
								stringvalidator.OneOf(dlpMcpScopeModes...),
							},
						},
						"principals": schema.SetNestedAttribute{
							MarkdownDescription: fmt.Sprintf("Groups and roles: 1 to %d, required for `ONLY` and "+
								"`EXCEPT` and not allowed for `ALL`.", dlpMcpMaxScopeEntries),
							Optional: true,
							Validators: []validator.Set{
								setvalidator.SizeBetween(1, dlpMcpMaxScopeEntries),
							},
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"principal_type": schema.StringAttribute{
										MarkdownDescription: "Kind of principal: `GROUP` or `ROLE`.",
										Required:            true,
										Validators: []validator.String{
											stringvalidator.OneOf(dlpPrincipalTypes...),
										},
									},
									"principal_id": schema.StringAttribute{
										MarkdownDescription: "IdP group or role name.",
										Required:            true,
										Validators: []validator.String{
											noSurroundingWhitespace,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// validateDlpMcpRules runs the plan-time checks the schema can't express on
// its own: scope mode/list pairing and mask_format only with the MASK action.
// Configurations that still hold unknown values in the rules are left for the
// API to check.
func validateDlpMcpRules(ctx context.Context, config tfsdk.Config, diags *diag.Diagnostics) {
	var list types.List
	diags.Append(config.GetAttribute(ctx, path.Root("rules"), &list)...)
	if diags.HasError() || list.IsNull() || list.IsUnknown() {
		return
	}
	var rules []dlpMcpRuleModel
	if d := list.ElementsAs(ctx, &rules, false); d.HasError() {
		// An unknown nested value can't decode into the model; the API
		// validates it at apply time instead.
		return
	}

	for i, rule := range rules {
		rulePath := path.Root("rules").AtListIndex(i)

		if scope := rule.AgentScope; scope != nil {
			if mode, ok := knownString(scope.Mode); ok {
				checkDlpMcpScopePairing(diags, rulePath.AtName("agent_scope").AtName("agent_ids"),
					"agent_scope", "agent_ids", mode, !scope.AgentIDs.IsNull())
			}
		}
		if scope := rule.PrincipalScope; scope != nil {
			if mode, ok := knownString(scope.Mode); ok {
				checkDlpMcpScopePairing(diags, rulePath.AtName("principal_scope").AtName("principals"),
					"principal_scope", "principals", mode, scope.Principals != nil)
			}
		}
		if rule.MaskFormat != nil {
			if action, ok := knownString(rule.Action); ok && action != dlpMcpMaskAction {
				diags.AddAttributeError(rulePath.AtName("mask_format"), "mask_format not allowed",
					fmt.Sprintf("`mask_format` is only valid with `action = %q`; this rule's action is %q.",
						dlpMcpMaskAction, action))
			}
		}
	}
}

func checkDlpMcpScopePairing(diags *diag.Diagnostics, p path.Path, block, list, mode string, listed bool) {
	switch {
	case mode == dlpMcpScopeAll && listed:
		diags.AddAttributeError(p, "List not allowed with mode ALL",
			fmt.Sprintf("`%s.%s` must be omitted when `%s.mode` is `ALL`.", block, list, block))
	case mode != dlpMcpScopeAll && !listed:
		diags.AddAttributeError(p, "Missing list",
			fmt.Sprintf("`%s.%s` must list at least one entry when `%s.mode` is `%s`.", block, list, block, mode))
	}
}

// --- wire shapes ----------------------------------------------------------------------

// dlpMcpRulePayload is one rule as the API takes and returns it. On a request,
// ID names a stored rule to keep (omitted creates a rule); on a response it is
// always set. The scopes are always sent explicitly — `{mode: ALL}` is what
// the API stores for an omitted scope anyway.
type dlpMcpRulePayload struct {
	ID                 *string                     `json:"id,omitempty"`
	Label              *string                     `json:"label,omitempty"`
	RuntimeBoundary    string                      `json:"runtime_boundary"`
	Action             string                      `json:"action"`
	MaskFormat         *dlpMcpMaskFormatPayload    `json:"mask_format,omitempty"`
	DryRun             bool                        `json:"dry_run"`
	AgentScope         dlpMcpAgentScopePayload     `json:"agent_scope"`
	PrincipalScope     dlpMcpPrincipalScopePayload `json:"principal_scope"`
	DetectionEngineIDs []string                    `json:"detection_engine_ids"`
}

type dlpMcpMaskFormatPayload struct {
	Keep               string `json:"keep"`
	KeepChars          int64  `json:"keep_chars"`
	MaskChar           string `json:"mask_char"`
	PreserveSeparators bool   `json:"preserve_separators"`
}

// The scope lists are always non-nil so they serialize as `[]`, never JSON
// null.
type dlpMcpAgentScopePayload struct {
	Mode     string   `json:"mode"`
	AgentIDs []string `json:"agent_ids"`
}

type dlpMcpPrincipalScopePayload struct {
	Mode       string                `json:"mode"`
	Principals []dlpPrincipalPayload `json:"principals"`
}

// dlpMcpPolicyResponse mirrors the API's McpPolicyResponse, returned for both
// ordinary MCP policies and the Baseline policy.
type dlpMcpPolicyResponse struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	IsBaseline   bool                `json:"is_baseline"`
	McpServerIDs []string            `json:"mcp_server_ids"`
	Rules        []dlpMcpRulePayload `json:"rules"`
	CreatedBy    string              `json:"created_by"`
	UpdatedBy    string              `json:"updated_by"`
	CreatedAt    string              `json:"created_at"`
	UpdatedAt    string              `json:"updated_at"`
}

// --- plan → wire ----------------------------------------------------------------------

// buildDlpMcpRulePayloads converts the planned rules to the API shape
// (non-nil, so an empty list serializes as `[]`).
func buildDlpMcpRulePayloads(ctx context.Context, rules []dlpMcpRuleModel) ([]dlpMcpRulePayload, error) {
	out := make([]dlpMcpRulePayload, 0, len(rules))
	for i, r := range rules {
		engineIDs, err := stringsFromList(ctx, r.DetectionEngineIDs)
		if err != nil {
			return nil, fmt.Errorf("rules[%d].detection_engine_ids: %w", i, err)
		}
		p := dlpMcpRulePayload{
			RuntimeBoundary:    r.RuntimeBoundary.ValueString(),
			Action:             r.Action.ValueString(),
			DryRun:             r.DryRun.ValueBool(),
			AgentScope:         dlpMcpAgentScopePayload{Mode: dlpMcpScopeAll, AgentIDs: []string{}},
			PrincipalScope:     dlpMcpPrincipalScopePayload{Mode: dlpMcpScopeAll, Principals: []dlpPrincipalPayload{}},
			DetectionEngineIDs: nonNilStrings(engineIDs),
		}
		if v, ok := knownString(r.Label); ok {
			p.Label = &v
		}
		if m := r.MaskFormat; m != nil {
			p.MaskFormat = &dlpMcpMaskFormatPayload{
				Keep:               m.Keep.ValueString(),
				KeepChars:          m.KeepChars.ValueInt64(),
				MaskChar:           m.MaskChar.ValueString(),
				PreserveSeparators: m.PreserveSeparators.ValueBool(),
			}
		}
		if s := r.AgentScope; s != nil {
			agentIDs, err := stringsFromSet(ctx, s.AgentIDs)
			if err != nil {
				return nil, fmt.Errorf("rules[%d].agent_scope.agent_ids: %w", i, err)
			}
			p.AgentScope = dlpMcpAgentScopePayload{Mode: s.Mode.ValueString(), AgentIDs: nonNilStrings(agentIDs)}
		}
		if s := r.PrincipalScope; s != nil {
			p.PrincipalScope = dlpMcpPrincipalScopePayload{
				Mode:       s.Mode.ValueString(),
				Principals: dlpPrincipalPayloads(s.Principals),
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// assignDlpMcpRuleIDs sets each planned rule's ID to a stored rule's, so a
// save keeps the rules it doesn't change instead of deleting and recreating
// them (which would churn rule ids and re-check every listed agent against
// the agent directory). A planned rule identical to a stored one keeps that
// rule's id wherever it moved; a rule that changed keeps the id of the stored
// rule at its position, if that one is still unclaimed. Anything left over is
// sent without an id and created.
func assignDlpMcpRuleIDs(planned, stored []dlpMcpRulePayload) {
	used := make([]bool, len(stored))
	storedKeys := make([]string, len(stored))
	for j := range stored {
		storedKeys[j] = dlpMcpRuleKey(stored[j])
	}

	matched := make([]bool, len(planned))
	for i := range planned {
		key := dlpMcpRuleKey(planned[i])
		for j := range stored {
			if !used[j] && stored[j].ID != nil && storedKeys[j] == key {
				planned[i].ID = stored[j].ID
				used[j], matched[i] = true, true
				break
			}
		}
	}
	for i := range planned {
		if !matched[i] && i < len(stored) && !used[i] && stored[i].ID != nil {
			planned[i].ID = stored[i].ID
			used[i] = true
		}
	}
}

// dlpMcpRuleKey renders a rule's content (everything but its id) in a
// canonical form, so a planned rule can be compared with a stored one.
func dlpMcpRuleKey(r dlpMcpRulePayload) string {
	r.ID = nil
	if r.Label != nil && *r.Label == "" {
		r.Label = nil
	}
	r.AgentScope.AgentIDs = slices.Sorted(slices.Values(nonNilStrings(r.AgentScope.AgentIDs)))
	principals := slices.Clone(r.PrincipalScope.Principals)
	slices.SortFunc(principals, func(a, b dlpPrincipalPayload) int {
		if c := strings.Compare(a.PrincipalType, b.PrincipalType); c != 0 {
			return c
		}
		return strings.Compare(a.PrincipalID, b.PrincipalID)
	})
	if principals == nil {
		principals = []dlpPrincipalPayload{}
	}
	r.PrincipalScope.Principals = principals
	r.DetectionEngineIDs = nonNilStrings(r.DetectionEngineIDs)
	b, _ := json.Marshal(r)
	return string(b)
}

// --- wire → state ------------------------------------------------------------------------

// applyDlpMcpRules maps the stored rules onto state. prior is the plan
// (Create/Update) or previous state (Read); the rule at the same position is
// used to settle omitted blocks back to null, since the API reports an
// omitted scope as an explicit `mode: ALL`. The result is never nil: `rules`
// is required, so even an empty rule set is a known empty list.
func applyDlpMcpRules(ctx context.Context, wire []dlpMcpRulePayload, prior []dlpMcpRuleModel) ([]dlpMcpRuleModel, error) {
	out := make([]dlpMcpRuleModel, 0, len(wire))
	for i, w := range wire {
		var p dlpMcpRuleModel
		if i < len(prior) {
			p = prior[i]
		}

		engineIDs, diags := types.ListValueFrom(ctx, types.StringType, nonNilStrings(w.DetectionEngineIDs))
		if diags.HasError() {
			return nil, fmt.Errorf("rules[%d].detection_engine_ids: %v", i, diags)
		}
		m := dlpMcpRuleModel{
			Label:              optionalStringFromPtr(w.Label, p.Label),
			RuntimeBoundary:    types.StringValue(w.RuntimeBoundary),
			Action:             types.StringValue(w.Action),
			DetectionEngineIDs: engineIDs,
			DryRun:             types.BoolValue(w.DryRun),
		}
		if f := w.MaskFormat; f != nil {
			m.MaskFormat = &dlpMcpMaskFormatModel{
				Keep:               types.StringValue(f.Keep),
				KeepChars:          types.Int64Value(f.KeepChars),
				MaskChar:           types.StringValue(f.MaskChar),
				PreserveSeparators: types.BoolValue(f.PreserveSeparators),
			}
		}

		agentScope, err := applyDlpMcpAgentScope(ctx, w.AgentScope, p.AgentScope)
		if err != nil {
			return nil, fmt.Errorf("rules[%d].agent_scope: %w", i, err)
		}
		m.AgentScope = agentScope
		m.PrincipalScope = applyDlpMcpPrincipalScope(w.PrincipalScope, p.PrincipalScope)
		out = append(out, m)
	}
	return out, nil
}

// applyDlpMcpAgentScope settles the API's default (mode ALL, no agents) back
// to an omitted block when the prior value had none.
func applyDlpMcpAgentScope(ctx context.Context, scope dlpMcpAgentScopePayload, prior *dlpMcpAgentScopeModel) (*dlpMcpAgentScopeModel, error) {
	if scope.Mode == dlpMcpScopeAll && len(scope.AgentIDs) == 0 && prior == nil {
		return nil, nil
	}
	priorIDs := types.SetNull(types.StringType)
	if prior != nil {
		priorIDs = prior.AgentIDs
	}
	agentIDs, err := setFromStrings(ctx, scope.AgentIDs, priorIDs)
	if err != nil {
		return nil, fmt.Errorf("agent_ids: %w", err)
	}
	return &dlpMcpAgentScopeModel{Mode: types.StringValue(scope.Mode), AgentIDs: agentIDs}, nil
}

// applyDlpMcpPrincipalScope is applyDlpMcpAgentScope for principal_scope.
func applyDlpMcpPrincipalScope(scope dlpMcpPrincipalScopePayload, prior *dlpMcpPrincipalScopeModel) *dlpMcpPrincipalScopeModel {
	if scope.Mode == dlpMcpScopeAll && len(scope.Principals) == 0 && prior == nil {
		return nil
	}
	var priorPrincipals []dlpPrincipalModel
	if prior != nil {
		priorPrincipals = prior.Principals
	}
	return &dlpMcpPrincipalScopeModel{
		Mode:       types.StringValue(scope.Mode),
		Principals: applyDlpPrincipals(scope.Principals, priorPrincipals),
	}
}

// --- errors ---------------------------------------------------------------------------

// addDlpMcpPolicyAPIError turns an MCP-policy API error into an actionable
// diagnostic. action is the failed verb phrase, e.g. "create the MCP policy".
func addDlpMcpPolicyAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusConflict:
		switch {
		case strings.Contains(apiErr.displayBody(), dlpMcpNotConvertedFragment):
			diags.AddError(
				"Organization is not on MCP server policies",
				fmt.Sprintf("Failed to %s: %s\n\nThis organization still manages MCP traffic through "+
					"enforcement policies, so the MCP policy API is read-only for it. Keep using "+
					"`barndoor_dlp_enforcement_policy` until the organization moves to MCP server policies.",
					action, apiErr.displayBody()),
			)
		case dlpErrorCode(apiErr) == dlpMcpServerAlreadyAssignedCode:
			diags.AddError(
				"MCP server already belongs to another policy",
				fmt.Sprintf("Failed to %s: %s\n\nEach MCP server can belong to only one MCP policy. Remove "+
					"the server from the other policy's `mcp_server_ids` first (in the same apply, Terraform "+
					"may need a `depends_on` so the removal happens before this change).",
					action, apiErr.displayBody()),
			)
		default:
			diags.AddError(
				"MCP policy conflicts with an existing one",
				fmt.Sprintf("Failed to %s: %s\n\nMCP policy names are unique per organization; choose a "+
					"different `name`.", action, apiErr.displayBody()),
			)
		}
	case http.StatusNotFound:
		diags.AddError(
			"Object not found by the Data Protection API",
			fmt.Sprintf("Failed to %s: %s\n\nConfirm every `detection_engine_ids` entry names a detection "+
				"engine that exists in the organization, and that MCP server policies are enabled for it.",
				action, apiErr.displayBody()),
		)
	case http.StatusUnprocessableEntity:
		// The API's validation message (rule number and what is wrong with
		// it) is the most specific thing we can show; surface it verbatim.
		diags.AddError("MCP policy rejected by the Data Protection API", apiErr.displayBody())
	case http.StatusServiceUnavailable:
		diags.AddError(
			"Agent directory unavailable",
			fmt.Sprintf("Failed to %s: %s\n\nThe Data Protection API could not check the listed agents "+
				"because the agent directory is temporarily unavailable. Retry the apply.",
				action, apiErr.displayBody()),
		)
	case http.StatusUnauthorized, http.StatusForbidden:
		diags.AddError(
			"Permission denied by the Data Protection API",
			fmt.Sprintf("Failed to %s: the configured credential was not accepted. Confirm the "+
				"service-account credential has a tenant-admin role and its token carries the organization "+
				"claim for the organization that owns this policy.\n\nServer message: %s",
				action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// dlpErrorCode extracts the `code` field of a JSON error body, or "".
func dlpErrorCode(apiErr *apiError) string {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(apiErr.body), &body); err != nil {
		return ""
	}
	return body.Code
}
