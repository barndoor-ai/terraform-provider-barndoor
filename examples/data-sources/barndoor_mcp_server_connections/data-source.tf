# Who still holds a connection to a connector we are retiring?
#
# Note this reads per-member data into Terraform state — see the data source
# documentation before adding it to a configuration whose state is widely
# readable.

data "barndoor_mcp_server" "legacy_github" {
  name = "GitHub (legacy)"
}

# Every principal still holding the connector, in any state: people, AI agents,
# and the tenant service account. An agent bound to a retiring server blocks
# the migration just as much as a person does.
data "barndoor_mcp_server_connections" "legacy_github" {
  server_id = data.barndoor_mcp_server.legacy_github.id
}

# Just the people to notify, and only those whose connection is live.
data "barndoor_mcp_server_connections" "to_notify" {
  server_id = data.barndoor_mcp_server.legacy_github.id
  status    = ["connected"]
  owner     = ["user"]
}

# `user_id` is the external identity-provider subject. Resolving it to a name or
# an email is a separate lookup against the identity API — no single endpoint
# returns users and their connections together.
output "legacy_github_user_ids" {
  description = "External IdP subjects of the people still on the legacy connector."
  value       = [for c in data.barndoor_mcp_server_connections.to_notify.connections : c.user_id]
}

output "legacy_github_holders" {
  description = "How many principals still hold the legacy connector, all classes included."
  value       = length(data.barndoor_mcp_server_connections.legacy_github.connections)
}
