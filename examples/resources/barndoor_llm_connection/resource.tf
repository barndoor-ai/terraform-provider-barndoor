# An API-key connection. The key is write-only: the platform stores it in its
# secret store and never returns it, so keep it out of committed
# configuration (a variable, or TF_VAR_*). Every provider that references the
# connection uses this key, so rotating it here rotates it for all of them.
resource "barndoor_llm_connection" "openai" {
  name           = "OpenAI production key"
  model_provider = "openai"
  base_url       = "https://api.openai.com" # no /v1: the gateway appends it
  api_key        = var.openai_api_key
}

# A Bedrock connection that assumes an IAM role instead of storing a key.
# The platform generates the external ID the role's trust policy must
# require; read it from effective_settings.
resource "barndoor_llm_connection" "bedrock" {
  name           = "Bedrock (us-east-1)"
  model_provider = "bedrock"
  auth_type      = "aws_role"
  base_url       = "https://bedrock-runtime.us-east-1.amazonaws.com"
  settings = jsonencode({
    region       = "us-east-1"
    iam_role_arn = "arn:aws:iam::123456789012:role/barndoor-bedrock"
  })
}

output "bedrock_trust_policy_external_id" {
  value = jsondecode(barndoor_llm_connection.bedrock.effective_settings).external_id
}

variable "openai_api_key" {
  type      = string
  sensitive = true
}
