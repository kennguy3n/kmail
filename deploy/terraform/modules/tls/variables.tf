variable "name" {
  description = "Logical name prefix for the issued certificates / secrets (e.g. kmail-prod)."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,40}$", var.name))
    error_message = "name must be 2-41 chars, lowercase alphanumeric and hyphens, not starting with a hyphen."
  }
}

variable "app_hostname" {
  description = "Public app hostname for the edge certificate, e.g. mail.example.com."
  type        = string
}

variable "api_hostname" {
  description = "Public API hostname added as a SAN on the edge certificate."
  type        = string
  default     = ""
}

variable "mail_hostname" {
  description = "Public mail hostname (MX target) added as a SAN on the edge certificate."
  type        = string
  default     = ""
}

variable "stalwart_server_name" {
  description = "Internal server name Stalwart presents on its mTLS listener (must match KMAIL_STALWART_TLS_SERVER_NAME and the Helm mtls config)."
  type        = string
  default     = "stalwart"
}

variable "public_issuer_ref" {
  description = "Reference to the public ACME issuer (cert-manager ClusterIssuer name, or ACME directory URL)."
  type        = string
  default     = "letsencrypt-prod"
}

variable "private_ca_issuer_ref" {
  description = "Reference to the private CA issuer (cert-manager ClusterIssuer) that signs the BFF<->Stalwart mTLS certs."
  type        = string
  default     = "kmail-internal-ca"
}

variable "tags" {
  description = "Tags/labels applied to the certificate resources."
  type        = map(string)
  default     = {}
}
