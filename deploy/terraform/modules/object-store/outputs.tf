output "bucket_name" {
  description = "Provisioned bucket name."
  value       = local.bucket_name
}

output "endpoint" {
  description = "Wasabi S3 endpoint for the bucket's region. Feed into KMAIL_ZK_FABRIC_S3_URL (or the attachment-store S3 endpoint)."
  value       = local.endpoint
}

output "s3_url" {
  description = "Fully-qualified bucket URL (endpoint + bucket)."
  value       = local.s3_url
}

output "region" {
  description = "Bucket region."
  value       = var.region
}
