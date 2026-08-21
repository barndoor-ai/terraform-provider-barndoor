# Publishing is a deliberate, one-way step: the server exists and is
# configurable before it, but end users only discover it after it.
#
# There are two preconditions, and the publish fails with a 422 until both
# hold:
#
#   1. The server is operationally available. Supplying credentials at create
#      (as below) activates it immediately; a server created without them
#      stays `pending` until someone connects it, and cannot be published.
#      Servers from `embedded`/`local` directory entries need no credentials.
#   2. It has at least one ACTIVE policy. Policies reference the server's id,
#      so they necessarily come after it — which is why publishing is its own
#      resource, ordered after the policies with depends_on, rather than a
#      flag on the server.
resource "barndoor_mcp_server" "github" {
  name                    = "GitHub"
  mcp_server_directory_id = "11111111-1111-1111-1111-111111111111"

  # Precondition 1: credentials activate the server, making it operationally
  # available and therefore publishable.
  client_id     = var.github_oauth_client_id
  client_secret = var.github_oauth_client_secret
}

# Precondition 2.
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

variable "github_oauth_client_id" {
  type = string
}

variable "github_oauth_client_secret" {
  type      = string
  sensitive = true
}
