# Import by organization ID (defaults to the "datadog-json" export type):
terraform import barndoor_log_export.audit 11111111-1111-1111-1111-111111111111

# Or import by organization ID and explicit export type:
terraform import barndoor_log_export.audit 11111111-1111-1111-1111-111111111111/datadog-json
