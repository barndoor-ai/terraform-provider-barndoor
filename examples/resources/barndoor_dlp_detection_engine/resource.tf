# A detection engine ("Protection Profile" in the app) binds a detection
# provider to the detection types it scans for. Engines are unique per
# (name, provider_type) — the same name across provider types renders as one
# merged profile in the app.
resource "barndoor_dlp_detection_engine" "pii" {
  name          = "Corporate PII"
  provider_type = "presidio"

  # Named Data Protection provider connection (for provider types that call
  # an external service); omit for built-in providers.
  provider_connection_name = "primary-presidio"

  enabled_detection_types = [
    "DETECTION_TYPE_EMAIL_ADDRESS",
    "DETECTION_TYPE_PHONE_NUMBER",
  ]

  # Provider-specific configuration; defaults to {} when omitted.
  config = jsonencode({ language = "en" })
}

# Engines are what enforcement policies reference.
resource "barndoor_dlp_enforcement_policy" "tokenize_pii" {
  name   = "Tokenize PII on MCP traffic"
  action = "POLICY_ACTION_TOKENIZE"

  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]

  detection_engine_ids = [barndoor_dlp_detection_engine.pii.id]
}
