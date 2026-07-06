# Import the organization's singleton IdP connection by the organization's ID.
# The credential's token claims determine the actual organization; Read
# replaces the imported id with the Keycloak IdP alias. The write-only
# client_id/client_secret cannot be read back — after import, the first plan
# re-sends the configured values (an in-place update).
terraform import barndoor_idp.sso 11111111-1111-1111-1111-111111111111
