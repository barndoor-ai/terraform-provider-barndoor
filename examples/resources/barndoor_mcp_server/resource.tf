# An MCP server instantiated from a directory entry, with tenant OAuth
# credentials. Creating it with credentials activates it immediately.
resource "barndoor_mcp_server" "github" {
  name                    = "GitHub (Engineering)"
  mcp_server_directory_id = "11111111-1111-1111-1111-111111111111"

  client_id     = var.github_oauth_client_id
  client_secret = var.github_oauth_client_secret

  # Override the directory's default OAuth scopes for this server.
  scopes = ["repo", "read:org"]
}

# A non-OAuth server seeded with pre-populated credentials that cascade to
# every user connection.
resource "barndoor_mcp_server" "internal_api" {
  name                    = "Internal API"
  mcp_server_directory_id = "22222222-2222-2222-2222-222222222222"

  prepopulated_credentials = jsonencode({
    api_key = var.internal_api_key
  })
  cascaded_fields = ["api_key"]
}
