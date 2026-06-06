output "host" {
  description = "Postgres endpoint hostname."
  value       = local.host
}

output "port" {
  description = "Postgres TCP port."
  value       = local.port
}

output "database_name" {
  description = "Initial database name."
  value       = var.database_name
}

output "dsn" {
  description = "Full Postgres connection DSN including the generated admin password. Feed into your secret manager / KMAIL_DATABASE_URL."
  value       = local.dsn
  sensitive   = true
}

output "admin_password" {
  description = "Generated admin password."
  value       = random_password.admin.result
  sensitive   = true
}
