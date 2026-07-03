# Connect an MCP server tenant-wide with a service-account-owned credential.
# Only non-OAuth credential providers are supported (api_key, bearer_token,
# basic_auth, generic) — OAuth servers require an interactive browser consent
# and must be connected out-of-band (e.g. in the Barndoor app).
resource "barndoor_mcp_server" "search" {
  name                    = "Search"
  mcp_server_directory_id = "00000000-0000-0000-0000-000000000000" # an api_key directory entry
}

resource "barndoor_connection" "search" {
  server_id = barndoor_mcp_server.search.id

  # Write-only: stored in Barndoor's secret store, never returned by the API.
  api_key = var.search_api_key
}

variable "search_api_key" {
  type      = string
  sensitive = true
}
