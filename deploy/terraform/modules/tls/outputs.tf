output "edge_secret_name" {
  description = "Kubernetes Secret name holding the public edge certificate (app/API/mail SANs)."
  value       = local.public_cert.secret
}

output "stalwart_server_secret_name" {
  description = "Secret name holding Stalwart's mTLS server certificate."
  value       = local.internal_certs.stalwart_server.secret
}

output "bff_client_secret_name" {
  description = "Secret name holding the BFF's mTLS client certificate (matches the Helm chart's mtls client secret)."
  value       = local.internal_certs.bff_client.secret
}

output "private_ca_issuer_ref" {
  description = "Private CA issuer used for the internal mTLS certs (feed into the Helm chart mtls.issuerRef)."
  value       = var.private_ca_issuer_ref
}
