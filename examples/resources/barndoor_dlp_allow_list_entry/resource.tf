# Suppress email-address findings for a shared support mailbox. The platform
# API has no update endpoint for allow-list entries, so changing any attribute
# replaces the entry.
resource "barndoor_dlp_allow_list_entry" "support_mailbox" {
  pattern      = "support@example.com"
  pattern_type = "PATTERN_TYPE_LITERAL"

  # Omit detection_types to suppress matches for every detection type.
  detection_types = ["DETECTION_TYPE_EMAIL"]

  reason = "Shared support mailbox, not PII"
}

# Regex entries allow whole families of values.
resource "barndoor_dlp_allow_list_entry" "test_cards" {
  pattern      = "^4111[ -]?1111[ -]?1111[ -]?1111$"
  pattern_type = "PATTERN_TYPE_REGEX"
  reason       = "Documented test card number"
}
