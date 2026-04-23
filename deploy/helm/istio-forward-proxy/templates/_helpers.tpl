{{/*
Expand the name of the chart.
*/}}
{{- define "forward-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name.
*/}}
{{- define "forward-proxy.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "forward-proxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "forward-proxy.labels" -}}
helm.sh/chart: {{ include "forward-proxy.chart" . }}
{{ include "forward-proxy.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "forward-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "forward-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "forward-proxy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "forward-proxy.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Bepaal de naam van de client-cert Secret. Als cert-manager is ingeschakeld
genereren we die zelf; anders verwachten we een bestaande Secret.
*/}}
{{- define "forward-proxy.certSecret" -}}
{{- if .Values.proxy.mtls.certManager.enabled -}}
{{- printf "%s-client-tls" (include "forward-proxy.fullname" .) -}}
{{- else -}}
{{- .Values.proxy.mtls.existingSecret -}}
{{- end -}}
{{- end -}}
