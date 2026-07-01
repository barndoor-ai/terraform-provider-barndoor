provider "barndoor" {
  # The platform host root, with no path — the provider appends each
  # service's API prefix itself.
  base_url  = "https://platform.barndoor.ai"
  token_url = "https://auth.barndoor.ai/realms/barndoor/protocol/openid-connect/token"
  client_id = "your-service-account-client-id"

  # client_secret and organization_id are best supplied via environment
  # variables (BARNDOOR_CLIENT_SECRET, BARNDOOR_ORGANIZATION_ID).
}

# Govern which AI Agents may call the Salesforce MCP server, allowing
# analysts to run read-style tools on small requests only. Destroying the
# resource archives the policy (the platform's terminal lifecycle state).
resource "barndoor_policy" "salesforce_read_only" {
  name          = "Salesforce read-only for analysts"
  mcp_server_id = "6f1c9312-8f3e-4bde-9c2b-b3a1d1cf34a4"

  description     = "Analysts may read Salesforce data; everything else is blocked."
  support_contact = "governance@example.com"
  status          = "ACTIVE" # DRAFT (default), ACTIVE, or INACTIVE

  # AI Agents this policy applies to.
  application_ids = [
    "a3a1f5c0-6a5d-4a4e-9a5b-2f6f0c1d2e3f",
  ]

  tags = ["salesforce", "governed"]

  rules = [
    {
      name   = "allow small reads"
      effect = "ALLOW"

      # actions and roles are required: the API would default an omitted
      # list to ["*"] (everything), so say it explicitly when you mean it.
      actions = ["search", "get_record", "list_records"]
      roles   = ["role:analyst", "group:data-team"]

      # Optional condition tree: each node has exactly one of `expr`
      # ({expr = "<expression>"}) or `all`/`any`/`none` ({of = [...]}).
      condition = jsonencode({
        all = {
          of = [
            { expr = { expr = "request.size < 100" } },
          ]
        }
      })
    },
  ]
}
