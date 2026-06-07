output "kubernetes_api_endpoint" {
  description = "Kubernetes API server endpoint."
  value       = module.kubernetes.api_endpoint
}

output "kubernetes_zones" {
  description = "Zones the cluster node pools span."
  value       = module.kubernetes.zones
}

output "postgres_dsn" {
  description = "Managed Postgres DSN (sensitive)."
  value       = module.postgres.dsn
  sensitive   = true
}

output "valkey_url" {
  description = "Managed Valkey URL (sensitive)."
  value       = module.valkey.url
  sensitive   = true
}

output "blob_bucket" {
  description = "Wasabi blob bucket name."
  value       = module.object_store.bucket_name
}

output "blob_endpoint" {
  description = "Wasabi S3 endpoint."
  value       = module.object_store.endpoint
}

output "edge_tls_secret" {
  description = "Secret name holding the public edge certificate."
  value       = module.tls.edge_secret_name
}

output "bff_client_tls_secret" {
  description = "Secret name holding the BFF mTLS client certificate."
  value       = module.tls.bff_client_secret_name
}

output "dns_records_managed" {
  description = "Number of DNS records managed."
  value       = module.dns.managed_record_count
}
