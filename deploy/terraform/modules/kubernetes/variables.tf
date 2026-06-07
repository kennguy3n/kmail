variable "cluster_name" {
  description = "Cluster name, e.g. kmail-prod-us-east-1."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,62}$", var.cluster_name))
    error_message = "cluster_name must be 2-63 chars, lowercase alphanumeric and hyphens, not starting with a hyphen."
  }
}

variable "kubernetes_version" {
  description = "Managed Kubernetes control-plane version."
  type        = string
  default     = "1.30"
}

variable "region" {
  description = "Provider region for the cluster."
  type        = string
  default     = ""
}

variable "availability_zones" {
  description = "Zones the node pools span. >=2 zones are required for the Helm chart's cross-AZ topology spread to provide HA."
  type        = list(string)
  default     = ["a", "b", "c"]

  validation {
    condition     = length(var.availability_zones) >= 2
    error_message = "Provide at least 2 availability zones for HA."
  }
}

variable "dns_zone" {
  description = "Internal DNS zone the API endpoint hostname is built under."
  type        = string
  default     = "kmail.internal"
}

variable "endpoint_override" {
  description = "Explicit API server endpoint to use instead of the derived one."
  type        = string
  default     = ""
}

variable "node_pools" {
  description = <<-EOT
    Map of node pools. Keyed by pool name. Each value:
      node_type  = provider instance flavor
      min_nodes  = autoscaler floor
      max_nodes  = autoscaler ceiling
      taint      = optional NoSchedule taint key (e.g. dedicated=stalwart) to
                   isolate Stalwart StatefulSets onto their own nodes.
    Default declares an api pool and a Stalwart-dedicated pool.
  EOT
  type = map(object({
    node_type = string
    min_nodes = number
    max_nodes = number
    taint     = optional(string, "")
  }))
  default = {
    api = {
      node_type = "4vcpu-16gb"
      min_nodes = 3
      max_nodes = 12
    }
    stalwart = {
      node_type = "8vcpu-32gb"
      min_nodes = 2
      max_nodes = 6
      taint     = "dedicated=stalwart"
    }
  }

  validation {
    condition     = alltrue([for p in values(var.node_pools) : p.min_nodes <= p.max_nodes])
    error_message = "every node pool must have min_nodes <= max_nodes."
  }
}

variable "tags" {
  description = "Tags/labels applied to the cluster and node pools."
  type        = map(string)
  default     = {}
}
