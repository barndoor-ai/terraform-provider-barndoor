# Rules for every MCP server that no barndoor_dlp_mcp_policy covers. The
# Baseline policy is a singleton: declare it once per organization.
# Destroying it clears its rules (the policy itself can't be deleted).
resource "barndoor_dlp_mcp_baseline_policy" "this" {
  rules = [
    {
      runtime_boundary     = "BOTH"
      action               = "POLICY_ACTION_REDACT"
      detection_engine_ids = ["00000000-0000-0000-0000-000000000001"]
    },
    {
      # Observe-only: record findings in tool output without acting.
      runtime_boundary     = "TOOL_OUTPUT"
      action               = "POLICY_ACTION_BLOCK"
      dry_run              = true
      detection_engine_ids = ["00000000-0000-0000-0000-000000000002"]
    },
  ]
}
