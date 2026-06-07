output "record_names" {
  description = "Map of logical record key to the FQDN managed."
  value       = { for k, v in local.records : k => v.name }
}

output "managed_record_count" {
  description = "Number of DNS records managed by this module."
  value       = length(local.records)
}

output "mail_records_managed" {
  description = "Whether the mail-delivery records (MX/SPF/DKIM/DMARC/...) are managed here."
  value       = var.manage_mail_records
}
