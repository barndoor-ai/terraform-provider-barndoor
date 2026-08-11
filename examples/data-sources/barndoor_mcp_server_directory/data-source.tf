# Look up a directory (catalog) entry by its connector slug — or by name or
# id — so the MCP server instance can reference it without a hand-copied UUID.
data "barndoor_mcp_server_directory" "github" {
  slug = "github"
}

resource "barndoor_mcp_server" "github" {
  name                    = "GitHub"
  mcp_server_directory_id = data.barndoor_mcp_server_directory.github.id
}
