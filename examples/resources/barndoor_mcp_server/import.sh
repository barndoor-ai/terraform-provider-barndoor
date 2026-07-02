# Import by the MCP server's UUID (the resource's id). Write-only attributes
# (client_id, client_secret, meta, prepopulated_credentials, cascaded_fields)
# are not recoverable from the API and read back null.
terraform import barndoor_mcp_server.example 11111111-1111-1111-1111-111111111111
