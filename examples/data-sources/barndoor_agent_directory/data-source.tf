# Look up an agent directory entry (the OAuth client definition) by display
# name — or by id — so the agent registration can reference it without a
# hand-copied UUID.
data "barndoor_agent_directory" "claude" {
  name = "Claude"
}

resource "barndoor_agent" "claude" {
  application_directory_id = data.barndoor_agent_directory.claude.id
  llm_gateway_enabled      = true
}
