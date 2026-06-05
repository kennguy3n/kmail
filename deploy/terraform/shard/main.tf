# ------------------------------------------------------------------
# deploy/terraform/shard — shard cell composition
# ------------------------------------------------------------------
# This module composes the resources that make up one shard. It is
# written provider-agnostically: the actual compute/managed-service
# resources live in per-environment child modules selected via the
# `*_module_source` style indirection in a root module. Here we model
# the shard as locals + a null_resource "assembly" marker so the module
# is `terraform validate`-clean without pinning a cloud, and so the
# outputs (consumed by the control plane) have a single source of truth.
#
# Replace the null_resource blocks with real `module "..."` / resource
# blocks for your provider; keep the locals + outputs contract intact so
# scripts/provision-shard.sh and internal/tenant.ExecShardProvisioner
# continue to parse the emitted JSON.

terraform {
  required_version = ">= 1.5.0"
}

locals {
  # Per-shard DNS-ish naming. A real root module maps these to actual
  # records / load-balancer hostnames.
  stalwart_host = "${var.shard_name}.stalwart.kmail.internal"
  postgres_host = "${var.shard_name}.pg.kmail.internal"
  meili_host    = "${var.shard_name}.meili.kmail.internal"
  valkey_host   = "${var.shard_name}.valkey.kmail.internal"

  stalwart_url = "https://${local.stalwart_host}:443"
  postgres_dsn = "postgres://kmail@${local.postgres_host}:5432/kmail?sslmode=require"

  common_tags = merge(var.tags, {
    "kmail.shard" = var.shard_name
    "kmail.role"  = "mailbox-shard"
  })
}

# Assembly marker: a stand-in for the compute/managed-service modules.
# Triggers on every input that changes the shard's identity so a
# re-apply with new sizing is detected. Swap for real resources.
resource "null_resource" "shard_assembly" {
  triggers = {
    shard_name          = var.shard_name
    stalwart_node_count = var.stalwart_node_count
    stalwart_type       = var.stalwart_instance_type
    postgres_type       = var.postgres_instance_type
    region              = var.region
  }
}
