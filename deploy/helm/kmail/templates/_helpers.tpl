{{/* Standard helpers shared across templates. */}}

{{- define "kmail.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kmail.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "kmail.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kmail.labels" -}}
app.kubernetes.io/name: {{ include "kmail.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "kmail.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kmail.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kmail.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kmail.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "kmail.secretName" -}}
{{- if .Values.secret.existingName -}}
{{- .Values.secret.existingName -}}
{{- else -}}
{{- printf "%s-secrets" (include "kmail.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
kmail.multiregionIngressAnnotations renders provider-specific
ExternalDNS annotations so a single chart install can publish both
the regional hostname (mail-<region>.<domain>) AND participate in
the global DNS-failover record (<globalHost>). The helper is a
no-op when `multiregion.enabled=false`, no provider is selected,
the region is empty, the base `domain` is empty, or `globalHost`
is empty — all four are required to produce well-formed annotations.

The regional hostname is constructed as `mail-<region>.<domain>`,
so a `domain` of `kmail.example.com` and region `us-west-2`
yields `mail-us-west-2.kmail.example.com`. `globalHost` is a
SEPARATE FQDN that points at the global failover record (e.g.
`mail.kmail.example.com`); ExternalDNS publishes both as a
comma-separated list because each provider supports multi-host
annotations natively. Using `globalHost` as the `domain` would
produce `mail-us-west-2.mail.kmail.example.com`, which is wrong.

Supported providers:
  - aws        Route 53 weighted + active-passive failover.
               Pairs with the `external-dns` chart's `--provider=aws`
               and a Hosted Zone shared across regions.
  - google     Cloud DNS weighted records.
  - cloudflare Cloudflare DNS load balancer (weighted pools).

All providers also receive a deterministic `set-identifier` so
weighted/failover groupings stay stable across helm upgrades.
*/}}
{{- define "kmail.multiregionIngressAnnotations" -}}
{{- $mr := .Values.multiregion -}}
{{- if and $mr.enabled $mr.externalDNSProvider $mr.region $mr.domain $mr.globalHost -}}
{{- $weight := default 100 $mr.dnsWeight -}}
external-dns.alpha.kubernetes.io/hostname: {{ printf "mail-%s.%s,%s" $mr.region $mr.domain $mr.globalHost | quote }}
external-dns.alpha.kubernetes.io/set-identifier: {{ $mr.region | quote }}
{{- if eq $mr.externalDNSProvider "aws" }}
external-dns.alpha.kubernetes.io/aws-weight: {{ $weight | quote }}
{{- with $mr.failoverRole }}
external-dns.alpha.kubernetes.io/aws-failover-type: {{ . | quote }}
{{- end }}
{{- else if eq $mr.externalDNSProvider "google" }}
external-dns.alpha.kubernetes.io/google-weight: {{ $weight | quote }}
{{- else if eq $mr.externalDNSProvider "cloudflare" }}
external-dns.alpha.kubernetes.io/cloudflare-load-balancer-weight: {{ $weight | quote }}
{{- end }}
{{- end -}}
{{- end -}}
