variable "name" {
  description = "Instance identifier, e.g. shard-1717 or control-plane. Used to name the resource and derive the hostname."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,62}$", var.name))
    error_message = "name must be 2-63 chars, lowercase alphanumeric and hyphens, not starting with a hyphen."
  }
}

variable "dns_zone" {
  description = "Internal DNS zone the instance hostname is built under (e.g. kmail.internal)."
  type        = string
  default     = "kmail.internal"
}

variable "endpoint_override" {
  description = "Explicit endpoint hostname to use instead of the derived <name>.pg.<dns_zone>. Set this to the managed provider's reported endpoint once known."
  type        = string
  default     = ""
}

variable "database_name" {
  description = "Initial database created on the instance."
  type        = string
  default     = "kmail"
}

variable "admin_username" {
  description = "Admin/owner role created on the instance."
  type        = string
  default     = "kmail"
}

variable "engine_version" {
  description = "PostgreSQL major.minor version."
  type        = string
  default     = "16"
}

variable "instance_class" {
  description = "Provider instance class/flavor for the database node."
  type        = string
  default     = "db-4vcpu-16gb"
}

variable "storage_gb" {
  description = "Allocated storage in GiB. Size for ~2KiB/msg metadata + WAL headroom; see docs/runbooks/capacity-planning.md."
  type        = number
  default     = 100

  validation {
    condition     = var.storage_gb >= 20
    error_message = "storage_gb must be at least 20."
  }
}

variable "ha_enabled" {
  description = "Provision a standby replica for automatic failover (multi-AZ). Recommended true for production shards."
  type        = bool
  default     = true
}

variable "backup_retention_days" {
  description = "Automated backup retention window in days."
  type        = number
  default     = 14

  validation {
    condition     = var.backup_retention_days >= 1 && var.backup_retention_days <= 35
    error_message = "backup_retention_days must be between 1 and 35."
  }
}

variable "sslmode" {
  description = "libpq sslmode for the emitted DSN. Production should be 'require' or stricter."
  type        = string
  default     = "require"

  validation {
    condition     = contains(["require", "verify-ca", "verify-full"], var.sslmode)
    error_message = "sslmode must be one of require, verify-ca, verify-full (production must not disable TLS)."
  }
}

variable "region" {
  description = "Provider region/zone to place the instance in."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Tags/labels applied to the instance for cost attribution."
  type        = map(string)
  default     = {}
}
