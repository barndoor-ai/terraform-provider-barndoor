# Read the organization's IdP settings — useful for gating other
# configuration on whether SSO is enforced. The settings themselves are
# read-only here: SSO enforcement and break-glass staging are interactive
# ceremonies performed in the Barndoor app.
data "barndoor_idp_settings" "current" {}

output "sso_enforced" {
  value = data.barndoor_idp_settings.current.enforce_sso
}
