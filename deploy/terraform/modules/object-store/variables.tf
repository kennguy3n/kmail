variable "name" {
  description = "Logical name for the bucket owner (e.g. kmail-prod). Used to derive the bucket name and for tagging."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,40}$", var.name))
    error_message = "name must be 2-41 chars, lowercase alphanumeric and hyphens, not starting with a hyphen."
  }
}

variable "bucket_name_override" {
  description = "Explicit bucket name. When empty a name of <name>-blobs-<random> is generated (S3 bucket names are globally unique)."
  type        = string
  default     = ""
}

variable "region" {
  description = "Wasabi region (e.g. us-east-1, us-west-1, eu-central-1). Drives the S3 endpoint host."
  type        = string
  default     = "us-east-1"
}

variable "versioning_enabled" {
  description = "Enable object versioning (recommended so deletes/overwrites are recoverable)."
  type        = bool
  default     = true
}

variable "object_lock_enabled" {
  description = "Enable S3 Object Lock (WORM) for compliance retention. Must be set at bucket creation time."
  type        = bool
  default     = false
}

variable "noncurrent_expiry_days" {
  description = "Days after which noncurrent object versions are permanently deleted by the lifecycle rule."
  type        = number
  default     = 30

  validation {
    condition     = var.noncurrent_expiry_days >= 1
    error_message = "noncurrent_expiry_days must be at least 1."
  }
}

variable "sse_algorithm" {
  description = "Server-side encryption algorithm for objects at rest (AES256 or aws:kms)."
  type        = string
  default     = "AES256"

  validation {
    condition     = contains(["AES256", "aws:kms"], var.sse_algorithm)
    error_message = "sse_algorithm must be AES256 or aws:kms."
  }
}

variable "tags" {
  description = "Tags applied to the bucket for cost attribution."
  type        = map(string)
  default     = {}
}
