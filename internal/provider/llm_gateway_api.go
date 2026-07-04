// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// llmGatewayAPIPrefix is the llm-gateway-service admin API mount point under
// the platform host root. The platform edge rewrites
// `/api/llm-gateway/admin/` to the service's `/admin/` prefix
// (`platform-lb` envoy route, bdai-platform PR #5422); llm-gateway enforces
// Keycloak JWT validation plus per-handler Cerbos authorization
// (`llm_gateway_*` resource policies) server-side.
const llmGatewayAPIPrefix = "api/llm-gateway/admin"

// llmGatewayTrafficTypes are the traffic lanes a governance policy can bind
// to. `all` matches both; `llm` is model-proxy traffic; `mcp` is MCP tool
// traffic.
var llmGatewayTrafficTypes = []string{"all", "llm", "mcp"}

// llmGatewayIdentityScopeTypes are the identity ("who") scope values the
// model-access and token-budget admin APIs accept. Rate limits additionally
// accept the legacy object scopes — see llmGatewayRateLimitScopeTypes.
var llmGatewayIdentityScopeTypes = []string{
	"org", "team", "user", "project", "api_key", "mcp_server", "agent", "role", "group",
}

// llmGatewayRateLimitScopeTypes adds the legacy object-dimension scopes that
// the rate-limit table's CHECK constraint still permits (V22).
var llmGatewayRateLimitScopeTypes = []string{
	"org", "team", "user", "project", "api_key", "mcp_server",
	"llm_provider", "model", "agent", "role", "group",
}

// llmGatewayErrorDetail extracts the human-readable message from an
// llm-gateway error body. The service renders errors in the OpenAI envelope
// (`{"error": {"message": "...", "type": "..."}}`), whose nested object the
// generic jsonErrorMessage helper cannot unwrap; fall back to the bounded
// raw body when the envelope is absent.
func llmGatewayErrorDetail(apiErr *apiError) string {
	body := strings.TrimSpace(apiErr.body)
	if body != "" && body[0] == '{' {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err == nil {
			if msg := strings.TrimSpace(envelope.Error.Message); msg != "" {
				return truncate(msg, maxErrorBodyLen)
			}
		}
	}
	return apiErr.displayBody()
}

// addLlmGatewayAPIError turns an llm-gateway admin API error into an
// actionable diagnostic. what names the resource kind (e.g. "LLM provider");
// action is the failed verb phrase (e.g. "create the LLM provider").
func addLlmGatewayAPIError(diags *diag.Diagnostics, what, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusBadRequest:
		// The API's validation message is the most specific thing we can
		// show; surface it verbatim.
		diags.AddError(what+" rejected by the LLM Gateway API", llmGatewayErrorDetail(apiErr))
	case http.StatusConflict:
		diags.AddError(what+" conflicts with an existing one", llmGatewayErrorDetail(apiErr))
	case http.StatusUnauthorized, http.StatusForbidden:
		diags.AddError(
			"Permission denied by the LLM Gateway API",
			fmt.Sprintf("Failed to %s: the configured credential was not accepted. The llm-gateway "+
				"admin API requires an admin-role service-account credential whose token carries the "+
				"organization claim (Cerbos `llm_gateway_*` policies).\n\nServer message: %s",
				action, llmGatewayErrorDetail(apiErr)),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// knownBool returns the value and true when v is a known, non-null bool.
func knownBool(v types.Bool) (bool, bool) {
	if v.IsNull() || v.IsUnknown() {
		return false, false
	}
	return v.ValueBool(), true
}

// int32PtrFromInt64 converts a known types.Int64 to a *int32 wire value; a
// null/unknown value converts to nil (key omitted / JSON null).
func int32PtrFromInt64(v types.Int64) *int32 {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	n := int32(v.ValueInt64())
	return &n
}

// int64FromInt32Ptr maps an optional wire int to state: nil means the value
// is unset (null).
func int64FromInt32Ptr(p *int32) types.Int64 {
	if p == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*p))
}
