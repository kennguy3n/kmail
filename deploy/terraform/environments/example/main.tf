# ------------------------------------------------------------------
# environments/example — reference root composition
# ------------------------------------------------------------------
# Wires the provider-agnostic building-block modules into one KMail
# environment: a Kubernetes cluster running the control plane, managed
# Postgres + Valkey, a Wasabi blob bucket, public + internal TLS, and
# the mail/app DNS records. Copy this directory per real environment
# (staging/, prod-us-east/, …), pin a backend, plug in concrete cloud
# providers, and set the variables.
#
# Validate (no cloud creds needed):
#   terraform -chdir=deploy/terraform/environments/example init -backend=false
#   terraform -chdir=deploy/terraform/environments/example validate

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = ">= 3.5.0"
    }
  }

  # Pin a real backend per environment, e.g.:
  # backend "s3" {
  #   bucket = "kmail-tfstate"
  #   key    = "prod/us-east-1/terraform.tfstate"
  #   region = "us-east-1"
  # }
}

locals {
  common_tags = {
    "kmail.env"     = var.environment
    "kmail.managed" = "terraform"
  }
}

module "kubernetes" {
  source = "../../modules/kubernetes"

  cluster_name       = "kmail-${var.environment}-${var.region}"
  kubernetes_version = "1.30"
  region             = var.region
  availability_zones = var.availability_zones
  dns_zone           = var.internal_dns_zone
  tags               = local.common_tags
}

module "postgres" {
  source = "../../modules/postgres"

  name                  = "kmail-${var.environment}"
  dns_zone              = var.internal_dns_zone
  database_name         = "kmail"
  engine_version        = "16"
  instance_class        = "db-4vcpu-16gb"
  storage_gb            = 200
  ha_enabled            = true
  backup_retention_days = 14
  sslmode               = "require"
  region                = var.region
  tags                  = local.common_tags
}

module "valkey" {
  source = "../../modules/valkey"

  name           = "kmail-${var.environment}"
  dns_zone       = var.internal_dns_zone
  engine_version = "8.0"
  node_type      = "cache-2vcpu-4gb"
  replica_count  = 2
  tls_enabled    = true
  region         = var.region
  tags           = local.common_tags
}

module "object_store" {
  source = "../../modules/object-store"

  name                   = "kmail-${var.environment}"
  region                 = var.wasabi_region
  versioning_enabled     = true
  noncurrent_expiry_days = 30
  sse_algorithm          = "AES256"
  tags                   = local.common_tags
}

module "tls" {
  source = "../../modules/tls"

  name                  = "kmail-${var.environment}"
  app_hostname          = var.app_hostname
  api_hostname          = var.api_hostname
  mail_hostname         = var.mail_host
  stalwart_server_name  = "stalwart"
  public_issuer_ref     = "letsencrypt-prod"
  private_ca_issuer_ref = "kmail-internal-ca"
  tags                  = local.common_tags
}

module "dns" {
  source = "../../modules/dns"

  zone_id           = var.dns_zone_id
  root_domain       = var.root_domain
  app_hostname      = var.app_hostname
  api_hostname      = var.api_hostname
  app_target        = var.ingress_hostname
  mail_host         = var.mail_host
  spf_include       = var.spf_include
  dkim_selector     = var.dkim_selector
  dkim_public_key   = var.dkim_public_key
  dmarc_policy      = "reject"
  reporting_mailbox = var.reporting_mailbox
}
