# Redact or block specific fields of an MCP server's tool outputs. The
# platform keeps at most one field control policy per MCP server; creating
# this resource fails if one already exists for the server (import it
# instead). Rules apply to tool output only (direction = "output").
resource "barndoor_dlp_field_control_policy" "crm_guard" {
  mcp_server_id = "22222222-2222-2222-2222-222222222222"
  name          = "CRM field controls"

  rules = jsonencode([
    {
      tool      = "search_contacts"
      direction = "output"
      fields = [
        { path = "records.email", action = "redact" },
        { path = "records.ssn", action = "block" },
      ]
      when = [
        { path = "records.classification", eq = "sensitive", action = "block", scope = "payload" },
      ]
      default_action = "pass"
    },
  ])
}

# Disable a policy without deleting its rules.
resource "barndoor_dlp_field_control_policy" "erp_guard" {
  mcp_server_id = "33333333-3333-3333-3333-333333333333"
  name          = "ERP field controls (staged)"
  enabled       = false

  rules = jsonencode([
    {
      tool      = "export_ledger"
      direction = "output"
      fields = [
        { path = "entries.account_number", action = "redact" },
      ]
    },
  ])
}
