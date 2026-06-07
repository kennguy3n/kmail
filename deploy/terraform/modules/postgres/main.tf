# ------------------------------------------------------------------
# modules/postgres — Managed PostgreSQL for a KMail shard / control plane
# ------------------------------------------------------------------
# Provider-agnostic by design (see deploy/terraform/README.md). The
# module declares the SHAPE of a managed Postgres instance — sizing,
# HA, backup window, network exposure — and emits the connection DSN
# the BFF / shard consume. Swap the `terraform_data.instance` marker
# for your provider's managed-database resource (aws_db_instance,
# google_sql_database_instance, azurerm_postgresql_flexible_server,
# digitalocean_database_cluster, …) keeping the locals + outputs intact.
#
# A strong password is generated here with the hermetic `random`
# provider so no secret is ever committed; surface it to your secret
# manager via the (sensitive) `dsn` output rather than logging it.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = ">= 3.5.0"
    }
  }
}

resource "random_password" "admin" {
  length  = 32
  special = true
  # Avoid characters that need URL-encoding in the DSN.
  override_special = "-_."
}

locals {
  port = 5432

  # Real providers expose the endpoint as an attribute; we model it as
  # a deterministic internal hostname so the contract is stable.
  host = coalesce(var.endpoint_override, "${var.name}.pg.${var.dns_zone}")

  dsn = format(
    "postgres://%s:%s@%s:%d/%s?sslmode=%s",
    var.admin_username,
    random_password.admin.result,
    local.host,
    local.port,
    var.database_name,
    var.sslmode,
  )

  tags = merge(var.tags, {
    "kmail.component" = "postgres"
    "kmail.name"      = var.name
  })
}

# Assembly marker — replace with the managed-database resource for your
# cloud. `triggers_replace` forces a new instance when identity-defining
# inputs change (so a re-apply with a bigger flavor is detected).
resource "terraform_data" "instance" {
  triggers_replace = {
    name              = var.name
    instance_class    = var.instance_class
    storage_gb        = var.storage_gb
    engine_version    = var.engine_version
    ha_enabled        = var.ha_enabled
    region            = var.region
    backup_retention  = var.backup_retention_days
    multi_az_failover = var.ha_enabled
  }
}
