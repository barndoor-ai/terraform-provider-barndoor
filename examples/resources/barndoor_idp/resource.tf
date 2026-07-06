# The organization's singleton enterprise SSO connection: OIDC federation
# with the company IdP (Okta, Entra ID, Auth0, ...). Creating this resource
# fails when SSO is already configured — `terraform import` the existing
# connection instead. Every attribute updates in place; nothing recreates the
# connection.
resource "barndoor_idp" "sso" {
  display_name = "Okta"
  issuer       = "https://company.okta.com"

  # OAuth client registered with the IdP for Barndoor. Both values are
  # write-only: the platform never returns them.
  client_id     = "barndoor-sso"
  client_secret = var.okta_client_secret

  authorization_url = "https://company.okta.com/oauth2/v1/authorize"
  token_url         = "https://company.okta.com/oauth2/v1/token"
  jwks_url          = "https://company.okta.com/oauth2/v1/keys"
  userinfo_url      = "https://company.okta.com/oauth2/v1/userinfo"

  # Request the groups claim so admin_group mapping works. Defaults to
  # "openid profile email" when unset.
  scopes = "openid profile email groups"

  # Users entering an @company.com email are redirected straight to the IdP.
  # Setting this updates the organization's domain on the platform.
  domain = "company.com"

  # Members of this IdP group get the Barndoor admin role on login.
  admin_group = "barndoor-admins"
}

variable "okta_client_secret" {
  type      = string
  sensitive = true
}
