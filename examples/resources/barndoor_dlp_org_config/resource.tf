# The organization's singleton Data Protection configuration. The platform
# provisions the row per organization; this resource adopts and configures it.
# `terraform destroy` resets both settings to the platform defaults
# (enabled = true, global_dry_run = false) rather than deleting anything.
resource "barndoor_dlp_org_config" "this" {
  enabled = true

  # Observe-only mode: every policy records findings without acting.
  global_dry_run = false
}
