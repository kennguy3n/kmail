# ------------------------------------------------------------------
# modules/dns — KMail DNS records (mail + app)
# ------------------------------------------------------------------
# Manages the public DNS a KMail tenant domain needs: the app/API
# hostnames plus the mail-delivery records (MX, SPF, DKIM, DMARC, MTA-STS,
# TLS-RPT, autoconfig/autodiscover). Provider-agnostic: replace the
# `terraform_data.record` marker with aws_route53_record /
# google_dns_record_set / cloudflare_record / azurerm_dns_*_record,
# iterating `local.records`, keeping the outputs contract intact.
#
# The DKIM/DMARC/SPF VALUES intentionally mirror what the BFF's DNS
# advisor emits (internal/config KMAIL_DNS_*) so the zone and the
# running service never drift.

terraform {
  required_version = ">= 1.5.0"
}

locals {
  # App / API surface.
  app_records = {
    app = {
      name    = var.app_hostname
      type    = "CNAME"
      ttl     = var.ttl
      records = [var.app_target]
    }
    api = {
      name    = var.api_hostname
      type    = "CNAME"
      ttl     = var.ttl
      records = [var.app_target]
    }
  }

  # Mail-delivery surface.
  mail_records = {
    mx = {
      name    = var.root_domain
      type    = "MX"
      ttl     = var.ttl
      records = ["10 ${var.mail_host}"]
    }
    spf = {
      name    = var.root_domain
      type    = "TXT"
      ttl     = var.ttl
      records = ["v=spf1 ${var.spf_include} -all"]
    }
    dkim = {
      name    = "${var.dkim_selector}._domainkey.${var.root_domain}"
      type    = "TXT"
      ttl     = var.ttl
      records = ["v=DKIM1; k=rsa; p=${var.dkim_public_key}"]
    }
    dmarc = {
      name    = "_dmarc.${var.root_domain}"
      type    = "TXT"
      ttl     = var.ttl
      records = ["v=DMARC1; p=${var.dmarc_policy}; rua=mailto:${var.reporting_mailbox}; ruf=mailto:${var.reporting_mailbox}; fo=1"]
    }
    mta_sts = {
      name    = "_mta-sts.${var.root_domain}"
      type    = "TXT"
      ttl     = var.ttl
      records = ["v=STSv1; id=${var.mta_sts_id}"]
    }
    tls_rpt = {
      name    = "_smtp._tls.${var.root_domain}"
      type    = "TXT"
      ttl     = var.ttl
      records = ["v=TLSRPTv1; rua=mailto:${var.reporting_mailbox}"]
    }
    autoconfig = {
      name    = "autoconfig.${var.root_domain}"
      type    = "CNAME"
      ttl     = var.ttl
      records = [var.app_target]
    }
    autodiscover = {
      name    = "autodiscover.${var.root_domain}"
      type    = "CNAME"
      ttl     = var.ttl
      records = [var.app_target]
    }
  }

  records = merge(local.app_records, var.manage_mail_records ? local.mail_records : {})
}

resource "terraform_data" "record" {
  for_each = local.records

  triggers_replace = {
    zone_id = var.zone_id
    name    = each.value.name
    type    = each.value.type
    ttl     = each.value.ttl
    records = join("|", each.value.records)
  }
}
