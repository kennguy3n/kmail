# ------------------------------------------------------------------
# deploy/terraform/shard — input variables
# ------------------------------------------------------------------
# A "shard" is one self-contained KMail mailbox cell: a small cluster
# of Stalwart mail nodes fronted by their own Postgres, Meilisearch,
# and Valkey. The control plane (internal/tenant.ShardService) registers
# the shard's endpoint and routes a slice of tenants to it. Scaling out
# = applying this module again with a new `shard_name`.
#
# This module is intentionally provider-light: it declares the shape of
# a shard (counts, sizes, networking inputs) and emits the connection
# outputs the control plane needs. Wire it to a concrete provider
# (libvirt / AWS / GCP / Hetzner …) in a thin root module per
# environment so this stays portable.

variable "shard_name" {
  description = "Unique shard identifier, e.g. shard-auto-1717. Used to name every resource and as the control-plane shard name."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,62}$", var.shard_name))
    error_message = "shard_name must be 2-63 chars, lowercase alphanumeric and hyphens, not starting with a hyphen."
  }
}

variable "stalwart_node_count" {
  description = "Number of Stalwart mail nodes in the shard (3 for HA: one primary + two replicas)."
  type        = number
  default     = 3

  validation {
    condition     = var.stalwart_node_count >= 1 && var.stalwart_node_count <= 9
    error_message = "stalwart_node_count must be between 1 and 9."
  }
}

variable "stalwart_instance_type" {
  description = "Provider instance/flavor for each Stalwart node."
  type        = string
  default     = "4vcpu-8gb"
}

variable "postgres_instance_type" {
  description = "Provider instance/flavor for the shard's Postgres node."
  type        = string
  default     = "4vcpu-16gb"
}

variable "max_mailboxes" {
  description = "Capacity hint persisted to stalwart_shards.max_mailboxes. The control plane stops routing new tenants here once current_mailboxes reaches this."
  type        = number
  default     = 10000

  validation {
    condition     = var.max_mailboxes > 0
    error_message = "max_mailboxes must be positive."
  }
}

variable "region" {
  description = "Provider region/zone to place the shard in."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Tags/labels applied to every resource for cost attribution and shard discovery."
  type        = map(string)
  default     = {}
}
