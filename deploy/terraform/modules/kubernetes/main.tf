# ------------------------------------------------------------------
# modules/kubernetes — Managed Kubernetes cluster for KMail
# ------------------------------------------------------------------
# Hosts the kmail-api Deployment + the per-shard Stalwart StatefulSets
# (deploy/helm/kmail). Provider-agnostic: replace `terraform_data`
# markers with eks/gke/aks/doks resources + their node pools, keeping
# the locals + outputs contract intact.
#
# The node pools are modelled as a map so a root module can declare an
# api pool and a (taint-isolated) stalwart pool with different flavors,
# matching the Helm chart's topologySpreadConstraints across zones.

terraform {
  required_version = ">= 1.5.0"
}

locals {
  cluster_name = var.cluster_name

  # The Helm chart spreads pods across these zones via
  # topology.kubernetes.io/zone; a real provider derives them from the
  # node pools. At least 2 zones are required for the cross-AZ spread to
  # mean anything.
  zones = var.availability_zones

  api_endpoint = coalesce(var.endpoint_override, "https://${var.cluster_name}.k8s.${var.dns_zone}")

  tags = merge(var.tags, {
    "kmail.component" = "kubernetes"
    "kmail.name"      = var.cluster_name
  })
}

resource "terraform_data" "cluster" {
  triggers_replace = {
    cluster_name = var.cluster_name
    version      = var.kubernetes_version
    region       = var.region
    zones        = join(",", var.availability_zones)
  }
}

# One marker per node pool. A real provider iterates the same map to
# create node groups.
resource "terraform_data" "node_pool" {
  for_each = var.node_pools

  triggers_replace = {
    cluster    = var.cluster_name
    pool       = each.key
    node_type  = each.value.node_type
    min_nodes  = each.value.min_nodes
    max_nodes  = each.value.max_nodes
    node_taint = try(each.value.taint, "")
  }
}
