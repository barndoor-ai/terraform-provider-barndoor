# The organization comes from the provider credential, so the import ID is
# not looked up; by convention use "baseline". Read replaces it with the
# Baseline policy's UUID. A Baseline policy that was never saved can't be
# imported; apply the resource instead.
terraform import barndoor_dlp_mcp_baseline_policy.this baseline
