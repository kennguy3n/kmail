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
kmail.stalwartMtlsUrl returns the HTTPS (jmap-tls / 8443) URL the BFF and
dispatch workers should use to reach Stalwart when mTLS is enabled, or an
empty string when this release should fall through to the ConfigMap value.

Three cases:
  1. stalwart.enabled: this release OWNS its StatefulSet. Target its own
     pod-0 via the headless Service. cert-manager bakes this exact hostname
     into the server cert SAN list (see stalwart-mtls.yaml), so the handshake
     verifies.
  2. else if stalwart.external.enabled: a control-plane-only release (the
     canary) targeting an EXISTING fleet owned by ANOTHER release. We
     synthesize the URL from the SAME `<fullname>-stalwart-<ordinal>.<svc>.
     <ns>.svc.cluster.local:8443` shape that the owning release uses for its
     own pods, driven by named, self-documenting fields. This guarantees the
     hostname matches a SAN on that fleet's server cert and stays well-formed
     — instead of a hand-built URL string that silently breaks the TLS
     handshake if a character is off. `fullname` is the owning release's
     `kmail.fullname` value (e.g. release `kmail` => `kmail-kmail`).
  3. else: "" — the operator supplies KMAIL_STALWART_URL via kmailApi.config
     and is responsible for SAN-covered hostname / mtls.serverName.
*/}}
{{- define "kmail.stalwartMtlsUrl" -}}
{{- $s := .Values.stalwart -}}
{{- if $s.enabled -}}
{{- printf "https://%s-stalwart-0.%s.%s.svc.cluster.local:8443" (include "kmail.fullname" .) $s.service.headlessName .Release.Namespace -}}
{{- else if and $s.external $s.external.enabled -}}
{{- $fullname := required "stalwart.external.fullname is required when stalwart.external.enabled=true (the kmail.fullname of the release that owns the Stalwart fleet, e.g. release `kmail` => `kmail-kmail`)" $s.external.fullname -}}
{{- $svc := $s.external.headlessName | default $s.service.headlessName -}}
{{- $ns := $s.external.namespace | default .Release.Namespace -}}
{{- /* hasKey, not `| default 0`: `default` treats the int zero-value as
       absent, so an explicit `ordinal: 0` would silently re-take the default.
       That's harmless while the default IS 0, but fragile if it ever changes,
       so resolve presence explicitly (mirrors the multiregion dnsWeight helper). */ -}}
{{- $ordinal := 0 -}}
{{- if hasKey $s.external "ordinal" -}}
{{- $ordinal = $s.external.ordinal -}}
{{- end -}}
{{- printf "https://%s-stalwart-%v.%s.%s.svc.cluster.local:8443" $fullname $ordinal $svc $ns -}}
{{- end -}}
{{- end -}}

{{/*
kmail.meshNameLabel emits ONLY app.kubernetes.io/name (the chart name, which is
shared by EVERY kmail release co-located in a namespace — production and its
canary both render `app.kubernetes.io/name: kmail`).

Use it ONLY in NetworkPolicy peer selectors (ingress `from:` / egress `to:`)
that must cross a RELEASE boundary within the namespace — specifically the
prod<->canary JMAP mesh: the canary's BFF/worker (instance=kmail-canary) speak
JMAP to the PRODUCTION Stalwart fleet (instance=kmail), and the production
Stalwart must accept them. Pinning `instance` in those peer selectors (as
kmail.selectorLabels does) would scope the allow-list to a single release and
silently blackhole the canary's mail path.

Every podSelector that GOVERNS a release's OWN pods (the `spec.podSelector` of
each policy) still uses kmail.selectorLabels (name+instance) so a policy only
ever governs its own release. mTLS (client certs from kmail-internal-ca) remains
the authentication layer for the JMAP hop; this selector is L3/L4
defense-in-depth scoped to kmail pods within the one namespace.
*/}}
{{- define "kmail.meshNameLabel" -}}
app.kubernetes.io/name: {{ include "kmail.name" . }}
{{- end -}}

{{/*
kmail.topologySpreadConstraints renders a list of TopologySpreadConstraint
entries, injecting a chart-correct `labelSelector` into any entry that does
not already define one.

WHY: `values.yaml` cannot call template functions, so a hand-written
`labelSelector.matchLabels` there has to hard-code `app.kubernetes.io/name:
kmail`. If an operator sets `nameOverride`/`fullnameOverride`, that literal
no longer matches the labels the chart actually stamps on the pods
(`kmail.selectorLabels`), so the spread silently selects nothing. By
defaulting the selector here from `kmail.selectorLabels` + the component, the
constraint always targets THIS release's pods of THIS component regardless of
any name override. An operator who supplies their own `labelSelector` in
values still wins (we only fill it in when absent).

Args (a dict):
  root        - the root context ($) so we can resolve selectorLabels
  component   - the app.kubernetes.io/component value to target
  constraints - the list from values (e.g. .Values.kmailApi.topologySpreadConstraints)
*/}}
{{- define "kmail.topologySpreadConstraints" -}}
{{- $root := .root -}}
{{- $component := .component -}}
{{- $out := list -}}
{{- range .constraints -}}
{{- $c := deepCopy . -}}
{{- if not $c.labelSelector -}}
{{- $labels := fromYaml (include "kmail.selectorLabels" $root) -}}
{{- $_ := set $labels "app.kubernetes.io/component" $component -}}
{{- $c = set $c "labelSelector" (dict "matchLabels" $labels) -}}
{{- end -}}
{{- $out = append $out $c -}}
{{- end -}}
{{- toYaml $out -}}
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
{{/*
  dnsWeight handling: Go templates' `default` returns the fallback
  for ANY zero value, including the integer 0. That collides with
  `dnsWeight: 0` (the documented region-drain knob — see
  `values.yaml`), so we resolve the value with an explicit
  nil-check that preserves an explicit zero. The chart's default
  weight (100) is applied only when dnsWeight is genuinely unset.
*/}}
{{- $weight := 100 -}}
{{- if hasKey $mr "dnsWeight" -}}
{{- $weight = $mr.dnsWeight -}}
{{- end -}}
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
