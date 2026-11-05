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

# "All except" scopes: agent_scope and principal_scope each default to
# applying to everyone when omitted. Set mode to EXCEPT to apply a rule to
# every agent, or every principal, other than the ones listed.
resource "barndoor_dlp_field_control_policy" "support_guard" {
  mcp_server_id = "44444444-4444-4444-4444-444444444444"
  name          = "Support tooling field controls"

  rules = jsonencode([
    {
      tool      = "search_tickets"
      direction = "output"
      # Every agent except the audit agent is restricted.
      agent_scope = {
        mode      = "EXCEPT"
        agent_ids = ["55555555-5555-5555-5555-555555555555"]
      }
      # Every principal except the support-leads group is restricted.
      principals = [
        { type = "GROUP", id = "support-leads" },
      ]
      principal_scope = { mode = "EXCEPT" }
      fields = [
        { path = "tickets.customer_email", action = "redact" },
      ]
    },
  ])
}
