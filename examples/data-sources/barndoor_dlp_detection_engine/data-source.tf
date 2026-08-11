# Look up an existing detection engine ("Protection Profile" in the app) by
# name to reference it without managing it. Names are only unique per
# provider_type — set provider_type (or use id) when one profile spans
# several engines.
data "barndoor_dlp_detection_engine" "pii" {
  name          = "Corporate PII"
  provider_type = "presidio"
}

# The looked-up engine feeds enforcement policies.
resource "barndoor_dlp_enforcement_policy" "tokenize_pii" {
  name   = "Tokenize PII on MCP traffic"
  action = "POLICY_ACTION_TOKENIZE"

  mcp_targets = [
    { mcp_server_id = "*", direction = "BOTH" },
  ]

  detection_engine_ids = [data.barndoor_dlp_detection_engine.pii.id]
}
