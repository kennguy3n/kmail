# KMail Terraform

Provider-agnostic infrastructure modules for KMail. The modules declare
the **shape** of each managed dependency (sizing, HA, networking,
security posture) and emit the connection contract the control plane /
Helm chart consume. Compute and managed-service resources are modelled
with built-in `terraform_data` markers (plus the hermetic `random`
provider for generated secrets) so the whole tree is
`terraform validate`-clean **without pinning a cloud or needing
credentials**. Swap the markers for your provider's real resources in a
thin per-environment root module — keep the `locals` + `outputs`
contracts intact and nothing downstream changes.

## Layout

```
deploy/terraform/
  modules/
    postgres/       Managed PostgreSQL (DSN + generated password)
    valkey/         Managed Valkey/Redis (URL + generated AUTH token)
    object-store/   Wasabi S3-compatible blob bucket
    kubernetes/     Managed Kubernetes cluster + node pools
    dns/            App + mail DNS records (MX/SPF/DKIM/DMARC/MTA-STS/TLS-RPT)
    tls/            Public edge + internal mTLS certificate wiring
  environments/
    example/        Reference root composing every module
  shard/            One mailbox shard cell (pre-existing)
```

## Validate locally

```bash
cd deploy/terraform
terraform fmt -recursive -check

# Per module / root:
for d in modules/* environments/example shard; do
  ( cd "$d" && terraform init -backend=false >/dev/null && terraform validate )
done
```

`terraform init -backend=false` only downloads the `random` provider
(used for generated secrets); no backend or cloud credentials are
required for validation.

## Going to a real cloud

1. Copy `environments/example` to `environments/<env>`.
2. Add the concrete provider block(s) (`aws`, `google`, `azurerm`,
   `cloudflare`, the Wasabi-pointed `aws` alias, cert-manager, …) and a
   `backend` for remote state.
3. In each module, replace the `terraform_data.*` marker with the real
   resource for your provider (the file header comments name the
   resource to use). Leave the `locals`/`outputs` alone so the
   `terraform output -json` contract that `scripts/provision-shard.sh`
   and `internal/tenant.ExecShardProvisioner` rely on stays stable.
4. Feed the sensitive outputs (`postgres_dsn`, `valkey_url`, the TLS
   secret names) into your secret manager — never into a committed
   `*.tfvars`.

Generated secrets live only in Terraform state; keep state in an
encrypted remote backend with locking.
