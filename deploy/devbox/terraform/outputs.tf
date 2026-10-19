output "github_environment_variables" {
  description = "Non-secret values for the devbox GitHub environment."
  value = {
    DEVBOX_GCP_PROJECT                 = var.project_id
    DEVBOX_GCP_WORKLOAD_IDENTITY       = google_iam_workload_identity_pool_provider.github.name
    DEVBOX_GCP_SERVICE_ACCOUNT         = google_service_account.ci.email
    DEVBOX_CONFIG_RECAPTCHA_SITE_KEY   = google_recaptcha_enterprise_key.devbox.name
    DEVBOX_CONFIG_RECAPTCHA_PROJECT_ID = var.project_id
  }
}

output "recaptcha_api_key" {
  description = "Server-side reCAPTCHA assessment key; store it as the DEVBOX_SECRET_RECAPTCHA_API_KEY environment secret."
  value       = google_apikeys_key.recaptcha.key_string
  sensitive   = true
}

output "box_service_account" {
  description = "Service account attached to every devbox VM."
  value       = google_service_account.box.email
}

output "network_contract" {
  description = "Rendered devbox network contract."
  value       = module.network.contract
}
