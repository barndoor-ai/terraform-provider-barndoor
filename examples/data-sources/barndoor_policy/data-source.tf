# Look up an existing access policy by name (or by id) to reference it from
# other configuration without managing it — for example to mirror its agent
# bindings onto a new policy, or to find the id for a `terraform import`.
data "barndoor_policy" "sales_guardrails" {
  name = "Sales guardrails"
}

output "sales_guardrails_status" {
  value = data.barndoor_policy.sales_guardrails.status
}

# The looked-up policy's agent bindings can seed other resources.
resource "barndoor_policy" "sales_guardrails_v2" {
  name            = "Sales guardrails v2"
  mcp_server_id   = data.barndoor_policy.sales_guardrails.mcp_server_id
  application_ids = data.barndoor_policy.sales_guardrails.application_ids

  rules = [{
    effect  = "ALLOW"
    actions = ["*"]
    roles   = ["group:sales"]
  }]
}
