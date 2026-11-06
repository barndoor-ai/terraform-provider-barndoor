# Protect tool traffic to the GitHub and Jira MCP servers. Each server can
# belong to only one MCP policy; servers no policy covers follow the Baseline
# policy (barndoor_dlp_mcp_baseline_policy).
resource "barndoor_dlp_mcp_policy" "engineering_tools" {
  name = "Engineering tools"
  # IDs of MCP servers registered in the organization (for example
  # barndoor_mcp_server.github.id).
  mcp_server_ids = ["github-server-id", "jira-server-id"]

  rules = [
    {
      # Tokenize PII in everything the tools return.
      runtime_boundary     = "TOOL_OUTPUT"
      action               = "POLICY_ACTION_TOKENIZE"
      detection_engine_ids = ["00000000-0000-0000-0000-000000000001"]
    },
    {
      # Mask secrets sent to the tools by contractors, keeping the last four
      # characters readable. The release agent is exempt.
      label                = "Mask secrets for contractors"
      runtime_boundary     = "TOOL_INPUT"
      action               = "POLICY_ACTION_MASK"
      detection_engine_ids = ["00000000-0000-0000-0000-000000000002"]

      mask_format = {
        keep       = "SUFFIX"
        keep_chars = 4
      }

      agent_scope = {
        mode      = "EXCEPT"
        agent_ids = ["33333333-3333-3333-3333-333333333333"]
      }

      principal_scope = {
        mode = "ONLY"
        principals = [
          { principal_type = "GROUP", principal_id = "contractors" },
        ]
      }
    },
  ]
}
