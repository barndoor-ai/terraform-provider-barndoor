provider "barndoor" {
  # The platform host root, with no path — the provider appends each
  # service's API prefix itself.
  base_url  = "https://platform.barndoor.ai"
  token_url = "https://auth.barndoor.ai/realms/barndoor/protocol/openid-connect/token"
  client_id = "your-service-account-client-id"

  # client_secret and organization_id are best supplied via environment
  # variables (BARNDOOR_CLIENT_SECRET, BARNDOOR_ORGANIZATION_ID) rather than
  # committed to configuration.
  organization_id = "your-keycloak-organization-uuid"
}
