# ------------------------------------------------------------------
# deploy/terraform/shard — outputs
# ------------------------------------------------------------------
# These outputs are the contract consumed by the control plane. The
# `shard` output is shaped to match the JSON that
# internal/tenant.ExecShardProvisioner parses (see
# scripts/provision-shard.sh), so `terraform output -json shard` feeds
# straight into shard registration.

output "shard" {
  description = "Connection details for the provisioned shard, in the shape ExecShardProvisioner expects."
  value = {
    name          = var.shard_name
    stalwart_url  = local.stalwart_url
    postgres_dsn  = local.postgres_dsn
    max_mailboxes = var.max_mailboxes
  }
}

output "stalwart_url" {
  description = "Primary Stalwart endpoint for the shard."
  value       = local.stalwart_url
}

output "postgres_dsn" {
  description = "Postgres DSN for the shard's dedicated database."
  value       = local.postgres_dsn
  sensitive   = true
}

output "meilisearch_host" {
  description = "Meilisearch host for the shard's search backend."
  value       = local.meili_host
}

output "valkey_host" {
  description = "Valkey host for the shard's cache/pubsub."
  value       = local.valkey_host
}
