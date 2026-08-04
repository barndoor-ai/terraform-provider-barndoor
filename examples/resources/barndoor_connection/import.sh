# Import by the *server's* UUID or slug — an organization holds at most one
# tenant-wide connection per server, so the connection is keyed by its server.
# Credential attributes are write-only and not recoverable from the API; they
# read back null.
terraform import barndoor_connection.search 11111111-1111-1111-1111-111111111111
terraform import barndoor_connection.search my-server-slug
