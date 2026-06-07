variable "name" {
  description = "Instance identifier, e.g. control-plane or shard-1717."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,62}$", var.name))
    error_message = "name must be 2-63 chars, lowercase alphanumeric and hyphens, not starting with a hyphen."
  }
}

variable "dns_zone" {
  description = "Internal DNS zone the endpoint hostname is built under."
  type        = string
  default     = "kmail.internal"
}

variable "endpoint_override" {
  description = "Explicit endpoint hostname to use instead of the derived <name>.valkey.<dns_zone>."
  type        = string
  default     = ""
}

variable "engine_version" {
  description = "Valkey/Redis engine version."
  type        = string
  default     = "8.0"
}

variable "node_type" {
  description = "Provider node type/flavor for each cache node."
  type        = string
  default     = "cache-2vcpu-4gb"
}

variable "replica_count" {
  description = "Number of read replicas (0 = single node, >=1 enables failover)."
  type        = number
  default     = 1

  validation {
    condition     = var.replica_count >= 0 && var.replica_count <= 5
    error_message = "replica_count must be between 0 and 5."
  }
}

variable "tls_enabled" {
  description = "Require in-transit TLS (emits a rediss:// URL). Production should keep this true."
  type        = bool
  default     = true
}

variable "region" {
  description = "Provider region/zone."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Tags/labels applied for cost attribution."
  type        = map(string)
  default     = {}
}
