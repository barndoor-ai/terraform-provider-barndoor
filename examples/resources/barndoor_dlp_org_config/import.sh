# Import the singleton by the organization's ID (the API also uses it as the
# configuration's id). The credential's token claims determine the actual
# organization; Read replaces the imported id with the authoritative value.
terraform import barndoor_dlp_org_config.this 11111111-1111-1111-1111-111111111111
