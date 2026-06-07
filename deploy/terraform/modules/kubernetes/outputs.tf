output "cluster_name" {
  description = "Cluster name."
  value       = local.cluster_name
}

output "api_endpoint" {
  description = "Kubernetes API server endpoint."
  value       = local.api_endpoint
}

output "zones" {
  description = "Availability zones the node pools span (consumed by the Helm chart topology spread)."
  value       = local.zones
}

output "node_pools" {
  description = "Map of provisioned node pool names to their declared sizing."
  value       = var.node_pools
}
