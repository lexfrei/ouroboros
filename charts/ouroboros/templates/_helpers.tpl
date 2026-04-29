{{/*
Expand the name of the chart.
*/}}
{{- define "ouroboros.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name (truncated to 63 chars).
*/}}
{{- define "ouroboros.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart label.
*/}}
{{- define "ouroboros.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "ouroboros.labels" -}}
helm.sh/chart: {{ include "ouroboros.chart" . }}
{{ include "ouroboros.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels for the controller.
*/}}
{{- define "ouroboros.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ouroboros.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Proxy fullname.
*/}}
{{- define "ouroboros.proxyFullname" -}}
{{ include "ouroboros.fullname" . }}-proxy
{{- end }}

{{/*
Proxy labels.
*/}}
{{- define "ouroboros.proxyLabels" -}}
helm.sh/chart: {{ include "ouroboros.chart" . }}
{{ include "ouroboros.proxySelectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Proxy selector labels.
*/}}
{{- define "ouroboros.proxySelectorLabels" -}}
app.kubernetes.io/name: {{ include "ouroboros.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: proxy
{{- end }}

{{/*
DaemonSet (etc-hosts) fullname.
*/}}
{{- define "ouroboros.etcHostsFullname" -}}
{{ include "ouroboros.fullname" . }}-etc-hosts
{{- end }}

{{/*
DaemonSet selector labels.
*/}}
{{- define "ouroboros.etcHostsSelectorLabels" -}}
app.kubernetes.io/name: {{ include "ouroboros.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: etc-hosts
{{- end }}

{{/*
DaemonSet labels.
*/}}
{{- define "ouroboros.etcHostsLabels" -}}
helm.sh/chart: {{ include "ouroboros.chart" . }}
{{ include "ouroboros.etcHostsSelectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "ouroboros.serviceAccountName" -}}
{{- default (include "ouroboros.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{/*
Image with tag (defaulting to chart appVersion).
*/}}
{{- define "ouroboros.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Proxy service FQDN passed to the controller for CoreDNS rewrites.
*/}}
{{- define "ouroboros.proxyFqdn" -}}
{{ include "ouroboros.proxyFullname" . }}.{{ .Release.Namespace }}.svc.cluster.local.
{{- end }}
