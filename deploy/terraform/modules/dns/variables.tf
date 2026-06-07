variable "zone_id" {
  description = "Provider DNS zone identifier the records are created in."
  type        = string
}

variable "root_domain" {
  description = "Apex domain for the tenant, e.g. example.com."
  type        = string
}

variable "app_hostname" {
  description = "FQDN for the web app, e.g. mail.example.com."
  type        = string
}

variable "api_hostname" {
  description = "FQDN for the API, e.g. api.example.com."
  type        = string
}

variable "app_target" {
  description = "CNAME target the app/API hostnames point at (the ingress/load-balancer hostname)."
  type        = string
}

variable "mail_host" {
  description = "Mail host the MX record points to, e.g. mx.example.com. Mirrors KMAIL_DNS_MAIL_HOST."
  type        = string
  default     = ""
}

variable "manage_mail_records" {
  description = "Whether to manage the MX/SPF/DKIM/DMARC/MTA-STS/TLS-RPT records here. Disable if mail DNS is managed elsewhere."
  type        = bool
  default     = true
}

variable "spf_include" {
  description = "SPF include mechanism, mirrors KMAIL_DNS_SPF_INCLUDE (e.g. include:_spf.kmail.app)."
  type        = string
  default     = ""
}

variable "dkim_selector" {
  description = "DKIM selector, mirrors KMAIL_DNS_DKIM_SELECTOR."
  type        = string
  default     = "kmail"
}

variable "dkim_public_key" {
  description = "DKIM public key (base64 p= value), mirrors KMAIL_DNS_DKIM_PUBLIC_KEY."
  type        = string
  default     = ""
}

variable "dmarc_policy" {
  description = "DMARC policy, mirrors KMAIL_DNS_DMARC_POLICY (none|quarantine|reject)."
  type        = string
  default     = "reject"

  validation {
    condition     = contains(["none", "quarantine", "reject"], var.dmarc_policy)
    error_message = "dmarc_policy must be none, quarantine, or reject."
  }
}

variable "reporting_mailbox" {
  description = "DMARC/TLS-RPT aggregate report mailbox, mirrors KMAIL_DNS_REPORTING_MAILBOX."
  type        = string
  default     = ""
}

variable "mta_sts_id" {
  description = "MTA-STS policy id (bump to force resolvers to refetch the policy)."
  type        = string
  default     = "1"
}

variable "ttl" {
  description = "TTL (seconds) for the managed records."
  type        = number
  default     = 300

  validation {
    condition     = var.ttl >= 60
    error_message = "ttl must be at least 60 seconds."
  }
}
