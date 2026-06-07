output "host" {
  description = "Valkey endpoint hostname."
  value       = local.host
}

output "port" {
  description = "Valkey TCP port."
  value       = local.port
}

output "url" {
  description = "Full Valkey connection URL including the generated AUTH token. Feed into KMAIL_VALKEY_URL."
  value       = local.url
  sensitive   = true
}

output "auth_token" {
  description = "Generated AUTH token."
  value       = random_password.auth.result
  sensitive   = true
}
