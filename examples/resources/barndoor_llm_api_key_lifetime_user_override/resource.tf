# One user's own override wins over their groups and the default, in either
# direction. This one lets a CI service user keep keys for up to a year.
resource "barndoor_llm_api_key_lifetime_user_override" "ci" {
  user_id           = "8c2f4a1e-5b6d-4e7f-9a0b-1c2d3e4f5a6b"
  max_lifetime_days = 365
}
