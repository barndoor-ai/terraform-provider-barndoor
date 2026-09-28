# Look up an MCP server, then list its policy revision history (newest first).
data "barndoor_mcp_server" "github" {
  slug = "github"
}

data "barndoor_policy_revisions" "github" {
  mcp_server_id = data.barndoor_mcp_server.github.id

  # Optional: only revisions made at or after this instant (RFC 3339).
  since = "2026-09-01T00:00:00Z"
}

output "latest_change_summary" {
  value = try(data.barndoor_policy_revisions.github.revisions[0].changes_summary, [])
}
