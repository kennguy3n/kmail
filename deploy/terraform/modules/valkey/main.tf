# ------------------------------------------------------------------
# modules/valkey — Managed Valkey/Redis for KMail
# ------------------------------------------------------------------
# Backs the BFF rate-limiter, cache and pub/sub. Provider-agnostic:
# swap `terraform_data.instance` for aws_elasticache_replication_group
# / google_redis_instance / azurerm_redis_cache / a Valkey operator,
# keeping the locals + outputs contract intact.
#
# A strong AUTH token is generated with the hermetic `random` provider
# and surfaced (sensitive) via the `url` output so it can flow into
# KMAIL_VALKEY_URL without being committed.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = ">= 3.5.0"
    }
  }
}

resource "random_password" "auth" {
  length  = 40
  special = false # Redis AUTH tokens are safest alphanumeric.
}

locals {
  port = 6379

  host = coalesce(var.endpoint_override, "${var.name}.valkey.${var.dns_zone}")

  # rediss:// (TLS) when in_transit_encryption is on, redis:// otherwise.
  scheme = var.tls_enabled ? "rediss" : "redis"

  url = format(
    "%s://:%s@%s:%d",
    local.scheme,
    random_password.auth.result,
    local.host,
    local.port,
  )

  tags = merge(var.tags, {
    "kmail.component" = "valkey"
    "kmail.name"      = var.name
  })
}

resource "terraform_data" "instance" {
  triggers_replace = {
    name           = var.name
    node_type      = var.node_type
    replica_count  = var.replica_count
    engine_version = var.engine_version
    tls_enabled    = var.tls_enabled
    region         = var.region
  }
}
