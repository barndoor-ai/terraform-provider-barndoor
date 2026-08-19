# Publishing is a deliberate, one-way step: the server exists and is
# configurable before it, but end users only discover it after it. The
# platform refuses to publish a server without an ACTIVE policy, and policies
# reference the server's id — so the publication is its own resource, ordered
# after the policies with depends_on.
resource "barndoor_mcp_server" "github" {
  name                    = "GitHub"
  mcp_server_directory_id = "11111111-1111-1111-1111-111111111111"
}

resource "barndoor_policy" "github" {
  name          = "github-default-access"
  mcp_server_id = barndoor_mcp_server.github.id
  status        = "ACTIVE"

  rules = [{
    name    = "allow all"
    effect  = "ALLOW"
    actions = ["*"]
    roles   = ["*"]
  }]
}

resource "barndoor_mcp_server_publication" "github" {
  mcp_server_id = barndoor_mcp_server.github.id
  depends_on    = [barndoor_policy.github] # publishing needs an ACTIVE policy
}
