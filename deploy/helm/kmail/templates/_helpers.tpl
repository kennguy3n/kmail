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
