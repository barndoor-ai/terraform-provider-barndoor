# Register an AI Agent (an agent directory entry) with the organization and
# opt it into the LLM Gateway.
resource "barndoor_agent" "claude" {
  application_directory_id = "11111111-1111-1111-1111-111111111111"

  llm_gateway_enabled = true
}

# The registration id is what access policies bind to.
resource "barndoor_policy" "claude_github" {
  name            = "Claude x GitHub"
  mcp_server_id   = barndoor_mcp_server.github.id
  application_ids = [barndoor_agent.claude.id]
  status          = "ACTIVE"

  rules = [{
    effect  = "ALLOW"
    actions = ["*"]
    roles   = ["*"]
  }]
}
