# Tokenize PII found in tool traffic to every MCP server, for members of the
# "engineering" group. target_kind is inferred (MCP_SERVER here); priority is
# assigned by the API when unset.
resource "barndoor_dlp_enforcement_policy" "mcp_pii" {
  name   = "Tokenize PII on MCP traffic"
  action = "POLICY_ACTION_TOKENIZE"

  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]

  principals = [
    { principal_type = "GROUP", principal_id = "engineering" },
  ]

  # Detection engines that evaluate the traffic (UUIDs of engines configured
  # in the organization).
  detection_engine_ids = ["00000000-0000-0000-0000-000000000000"]
}

# Block findings in prompts sent to a specific model provider.
resource "barndoor_dlp_enforcement_policy" "llm_prompts" {
  name          = "Block secrets in prompts"
  action        = "POLICY_ACTION_BLOCK"
  provider_ids  = ["openai"]
  runtime_stage = "RUNTIME_STAGE_PROMPT"

  # Roll out observing first; flip to false to enforce.
  dry_run = true

  detection_engine_ids = ["00000000-0000-0000-0000-000000000000"]
}

# Limit a policy to two specific agents instead of every agent. Omitting
# agent_scope (as in the examples above) applies the policy to every agent,
# including agents created later.
resource "barndoor_dlp_enforcement_policy" "agent_pilot" {
  name   = "Tokenize PII for pilot agents"
  action = "POLICY_ACTION_TOKENIZE"

  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]

  agent_scope = {
    mode = "SELECTED"
    agent_ids = [
      "00000000-0000-0000-0000-000000000001",
      "00000000-0000-0000-0000-000000000002",
    ]
  }

  detection_engine_ids = ["00000000-0000-0000-0000-000000000000"]
}
