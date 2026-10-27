# The organization's default lifetime for API keys users create for
# themselves: every self-serve key gets exactly 60 days, and users don't pick
# an expiry. Keys created by admins are exempt. `terraform destroy` clears the
# default.
resource "barndoor_llm_api_key_lifetime_policy" "org" {
  max_lifetime_days = 60
  fixed_lifetime    = true
}
