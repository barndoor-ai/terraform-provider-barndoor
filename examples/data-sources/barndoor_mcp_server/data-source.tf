# Look up an existing MCP server by name, slug, or id to reference it without
# managing it — most commonly to attach a policy to a server that was
# onboarded outside this configuration.
data "barndoor_mcp_server" "github" {
  name = "GitHub"
}

resource "barndoor_policy" "github_readonly" {
  name          = "GitHub read-only"
  mcp_server_id = data.barndoor_mcp_server.github.id

  rules = [{
    effect  = "DENY"
    actions = ["create_*", "update_*", "delete_*"]
    roles   = ["*"]
  }]
}
