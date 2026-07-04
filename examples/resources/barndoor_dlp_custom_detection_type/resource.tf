# Detect internal project codenames. Patterns are evaluated in order; the
# platform assigns the type's wire name (DETECTION_TYPE_CUSTOM_…), exposed as
# `detection_type` for use in allow-list scoping and activity filters.
resource "barndoor_dlp_custom_detection_type" "codenames" {
  name        = "Project codenames"
  description = "Internal project codenames that must not leave the organization"

  patterns = [
    { pattern = "(?i)project\\s+aurora", pattern_type = "PATTERN_TYPE_REGEX" },
    { pattern = "AURORA-CLASSIFIED", pattern_type = "PATTERN_TYPE_LITERAL" },
  ]

  # Both default to *_MEDIUM when omitted.
  default_severity   = "DETECTION_SEVERITY_HIGH"
  default_confidence = "DETECTION_CONFIDENCE_HIGH"
}

# The generated wire name plugs into other Data Protection resources.
resource "barndoor_dlp_allow_list_entry" "codename_docs_site" {
  pattern         = "aurora.docs.example.com"
  pattern_type    = "PATTERN_TYPE_LITERAL"
  detection_types = [barndoor_dlp_custom_detection_type.codenames.detection_type]
  reason          = "Public documentation host, not a leak"
}
