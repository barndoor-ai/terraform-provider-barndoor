# A service key bound to an IdP group, so the group's budgets, rate limits and
# model access apply to it. Its secret is in `key` (sensitive, and stored in
# Terraform state): pass it on, for example to your secrets manager.
resource "barndoor_llm_api_key" "billing" {
  name       = "billing-service"
  group_name = "finance"
  expires_at = "2027-06-30T23:59:59Z"

  # Change this value to rotate the secret in place: the key keeps its id,
  # agent binding and usage history. A `time_rotating` resource's id works
  # well here for scheduled rotation. The old secret keeps working for an
  # hour so clients can move over.
  rotation_trigger = {
    rotated = "2026-11"
  }
  grace_period_secs = 3600
}

# A key assigned to one user: their identity, groups and policies apply.
resource "barndoor_llm_api_key" "ada_laptop" {
  name    = "ada-laptop"
  user_id = "8c2f4a1e-5b6d-4e7f-9a0b-1c2d3e4f5a6b"
}
