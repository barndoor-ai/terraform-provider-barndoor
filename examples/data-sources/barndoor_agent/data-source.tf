# Look up an existing AI Agent registration by display name (or by id) to
# reference it without managing it — most commonly to bind policies to an
# agent that was registered outside this configuration.
data "barndoor_agent" "claude" {
  name = "Claude"
}

resource "barndoor_policy" "claude_guardrails" {
  name            = "Claude guardrails"
  mcp_server_id   = "00000000-0000-0000-0000-000000000000" # your server id
  application_ids = [data.barndoor_agent.claude.id]

  rules = [{
    effect  = "ALLOW"
    actions = ["*"]
    roles   = ["*"]
  }]
}
