# Interns' self-serve keys get exactly a week; they don't choose.
resource "barndoor_llm_api_key_lifetime_role_override" "intern" {
  role_name         = "intern"
  max_lifetime_days = 7
  fixed_lifetime    = true
}

# An override with no limits exempts the role from the default.
resource "barndoor_llm_api_key_lifetime_role_override" "admin" {
  role_name = "admin"
}
