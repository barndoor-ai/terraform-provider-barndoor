# Import an existing notification channel by its server-assigned id.
#
# Note: `signing_secret` cannot be recovered on import — the platform reveals it
# exactly once and Terraform was not the recipient — so it stays null until the
# next rotation via `rotate_when_changed`. `has_signing_secret` still reports
# whether the platform holds one.
terraform import barndoor_notification_channel.security_webhook 3fa85f64-5717-4562-b3fc-2c963f66afa6
