# Contractors' self-serve keys expire within a week; users choose when.
resource "barndoor_llm_api_key_lifetime_group_override" "contractors" {
  group_name        = "contractors"
  max_lifetime_days = 7
}

# An override with no limits exempts its group from the default.
resource "barndoor_llm_api_key_lifetime_group_override" "platform" {
  group_name = "platform"
}
