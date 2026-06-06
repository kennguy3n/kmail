variable "environment" {
  description = "Environment name, e.g. staging or prod."
  type        = string
  default     = "staging"
}

variable "region" {
  description = "Primary cloud region for compute + managed data services."
  type        = string
  default     = "us-east-1"
}

variable "availability_zones" {
  description = "Zones the Kubernetes node pools span (>=2 for HA)."
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b", "us-east-1c"]
}

variable "wasabi_region" {
  description = "Wasabi region for the blob bucket."
  type        = string
  default     = "us-east-1"
}

variable "internal_dns_zone" {
  description = "Internal DNS zone for service hostnames."
  type        = string
  default     = "kmail.internal"
}

variable "dns_zone_id" {
  description = "Public DNS zone id the app/mail records are created in."
  type        = string
  default     = "ZONE_ID_PLACEHOLDER"
}

variable "root_domain" {
  description = "Apex tenant domain."
  type        = string
  default     = "example.com"
}

variable "app_hostname" {
  description = "Public web app hostname."
  type        = string
  default     = "mail.example.com"
}

variable "api_hostname" {
  description = "Public API hostname."
  type        = string
  default     = "api.example.com"
}

variable "ingress_hostname" {
  description = "Ingress/load-balancer hostname the app/API CNAMEs target."
  type        = string
  default     = "ingress.example.com"
}

variable "mail_host" {
  description = "Mail host the MX record targets."
  type        = string
  default     = "mx.example.com"
}

variable "spf_include" {
  description = "SPF include mechanism."
  type        = string
  default     = "include:_spf.example.com"
}

variable "dkim_selector" {
  description = "DKIM selector."
  type        = string
  default     = "kmail"
}

variable "dkim_public_key" {
  description = "DKIM public key (base64 p= value)."
  type        = string
  default     = "PLACEHOLDER_DKIM_PUBLIC_KEY"
}

variable "reporting_mailbox" {
  description = "DMARC/TLS-RPT report mailbox."
  type        = string
  default     = "dmarc-reports@example.com"
}
