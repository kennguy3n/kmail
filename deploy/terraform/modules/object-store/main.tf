# ------------------------------------------------------------------
# modules/object-store — Wasabi (S3-compatible) bucket for KMail blobs
# ------------------------------------------------------------------
# KMail stores message bodies / attachments in an S3-compatible bucket
# (Wasabi in production; the zk-object-fabric emulator in dev). Wasabi
# speaks the S3 API, so this is driven by the standard `aws` provider
# pointed at a Wasabi region endpoint:
#
#   provider "aws" {
#     alias      = "wasabi"
#     region     = "us-east-1"
#     access_key = var.wasabi_access_key
#     secret_key = var.wasabi_secret_key
#     endpoints { s3 = "https://s3.us-east-1.wasabisys.com" }
#     skip_credentials_validation = true
#     skip_region_validation      = true
#     skip_requesting_account_id  = true
#   }
#
# To keep `terraform validate` hermetic and cloud-free in this repo the
# bucket itself is modelled with a `terraform_data` marker plus the
# `random` provider for the bucket suffix. Replace the marker with
# `aws_s3_bucket` + `aws_s3_bucket_versioning` +
# `aws_s3_bucket_lifecycle_configuration` +
# `aws_s3_bucket_server_side_encryption_configuration` and pass the
# aliased provider, keeping the locals + outputs contract intact.

terraform {
  required_version = ">= 1.5.0"
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = ">= 3.5.0"
    }
  }
}

# Globally-unique-ish bucket suffix so re-creates don't collide.
resource "random_id" "suffix" {
  byte_length = 4
}

locals {
  bucket_name = var.bucket_name_override != "" ? var.bucket_name_override : "${var.name}-blobs-${random_id.suffix.hex}"

  endpoint = "https://s3.${var.region}.wasabisys.com"
  s3_url   = "${local.endpoint}/${local.bucket_name}"

  tags = merge(var.tags, {
    "kmail.component" = "object-store"
    "kmail.name"      = var.name
  })
}

resource "terraform_data" "bucket" {
  triggers_replace = {
    bucket_name           = local.bucket_name
    region                = var.region
    versioning            = var.versioning_enabled
    object_lock           = var.object_lock_enabled
    lifecycle_expiry_days = var.noncurrent_expiry_days
    sse_algorithm         = var.sse_algorithm
  }
}
