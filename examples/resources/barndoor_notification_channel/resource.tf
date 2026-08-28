# A webhook channel. The platform generates a Standard Webhooks signing secret
# and reveals it exactly once, on creation — it is stored in Terraform state so
# it can be consumed downstream, and cannot be read back from the API.
resource "barndoor_notification_channel" "security_webhook" {
  type = "webhook"
  url  = "https://hooks.example.com/barndoor"

  # The API replaces this set rather than merging, so it is the complete
  # desired state. An empty set means the channel delivers nothing.
  subscriptions = [
    "break_glass_used",
    "policy_changed",
  ]
}

# Rotate the signing secret every 30 days. Unlike most providers' equivalent
# argument, this rotates IN PLACE: the channel id, subscriptions and delivery
# configuration are all preserved, because the API exposes a dedicated rotate
# endpoint rather than requiring the channel be recreated.
resource "time_rotating" "webhook_secret" {
  rotation_days = 30
}

resource "barndoor_notification_channel" "rotating_webhook" {
  type          = "webhook"
  url           = "https://hooks.example.com/barndoor-rotating"
  subscriptions = ["break_glass_used"]

  rotate_when_changed = {
    rotated_at = time_rotating.webhook_secret.rotation_rfc3339
  }
}

# Feed the secret to whatever verifies delivered payloads.
output "webhook_signing_secret" {
  value     = barndoor_notification_channel.rotating_webhook.signing_secret
  sensitive = true
}

# An email channel — any deliverable address, not necessarily a platform user.
resource "barndoor_notification_channel" "ops_email" {
  type          = "email"
  email_address = "security-ops@example.com"
  subscriptions = ["connection_broken"]
}

# A Slack channel. The organization's Slack app must already be installed —
# that is an interactive consent flow and is not manageable through this API.
resource "barndoor_notification_channel" "slack_alerts" {
  type             = "slack"
  label            = "#security-alerts"
  slack_channel_id = "C0123456789"
  subscriptions    = ["break_glass_used"]
}

# A Microsoft Teams channel. teams_workflow_url is write-only: the API stores
# it as a secret and never returns it, so out-of-band changes are not detected.
resource "barndoor_notification_channel" "teams_alerts" {
  type               = "teams"
  label              = "Security Ops"
  teams_workflow_url = "https://example.logic.azure.com/workflows/REDACTED"
  subscriptions      = ["break_glass_used"]
}

# Suspend delivery without destroying the channel or its subscriptions.
resource "barndoor_notification_channel" "paused" {
  type          = "email"
  email_address = "noisy@example.com"
  enabled       = false
  subscriptions = ["mcp_server_added"]
}
