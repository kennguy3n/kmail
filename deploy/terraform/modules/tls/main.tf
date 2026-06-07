# ------------------------------------------------------------------
# modules/tls — TLS certificate issuance for KMail
# ------------------------------------------------------------------
# Declares the certificates KMail needs and WHERE they are issued, in a
# provider-agnostic way. There are two distinct trust domains:
#
#   1. PUBLIC edge certs (app/API/mail hostnames) — issued by a public
#      ACME CA (Let's Encrypt / ZeroSSL) via cert-manager in-cluster or
#      the `acme` Terraform provider. These chain to a public root.
#
#   2. INTERNAL mTLS (BFF <-> Stalwart) — issued by a PRIVATE CA managed
#      by cert-manager (a ClusterIssuer backed by a CA secret). The Helm
#      chart consumes these via mtls.issuerRef (see deploy/helm/kmail).
#
# This module does NOT mint private keys into Terraform state (an
# anti-pattern). Instead it models the desired certificates + issuer
# wiring with `terraform_data` markers and emits the issuer/secret
# references the rest of the platform binds to. Replace the markers with
# cert-manager `Certificate`/`Issuer` manifests (kubernetes_manifest) or
# `acme_certificate` resources, keeping the outputs contract intact.

terraform {
  required_version = ">= 1.5.0"
}

locals {
  # Public edge certificate covering the app/API/mail SANs.
  public_cert = {
    common_name = var.app_hostname
    sans = compact([
      var.app_hostname,
      var.api_hostname,
      var.mail_hostname,
    ])
    issuer     = var.public_issuer_ref
    secret     = "${var.name}-edge-tls"
    renew_acme = true
  }

  # Internal mTLS materials issued by the private CA.
  internal_certs = {
    stalwart_server = {
      common_name = var.stalwart_server_name
      sans        = [var.stalwart_server_name]
      usage       = "server"
      secret      = "${var.name}-stalwart-server-tls"
    }
    bff_client = {
      common_name = "${var.name}-bff"
      sans        = []
      usage       = "client"
      secret      = "${var.name}-stalwart-client-tls"
    }
  }

  tags = merge(var.tags, {
    "kmail.component" = "tls"
    "kmail.name"      = var.name
  })
}

resource "terraform_data" "public_cert" {
  triggers_replace = {
    common_name = local.public_cert.common_name
    sans        = join(",", local.public_cert.sans)
    issuer      = local.public_cert.issuer
  }
}

resource "terraform_data" "internal_cert" {
  for_each = local.internal_certs

  triggers_replace = {
    name        = each.key
    common_name = each.value.common_name
    usage       = each.value.usage
    issuer      = var.private_ca_issuer_ref
  }
}
