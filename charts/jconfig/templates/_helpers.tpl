{{- define "jconfig.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "jconfig.fullname" -}}
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

{{- define "jconfig.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "jconfig.labels" -}}
helm.sh/chart: {{ include "jconfig.chart" . }}
{{ include "jconfig.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "jconfig.selectorLabels" -}}
app.kubernetes.io/name: {{ include "jconfig.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "jconfig.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "jconfig.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "jconfig.configMapName" -}}
{{- default (include "jconfig.fullname" .) .Values.existingConfigMap -}}
{{- end -}}

{{- define "jconfig.knownHostsSecretName" -}}
{{- if .Values.existingKnownHostsSecret -}}
{{- .Values.existingKnownHostsSecret -}}
{{- else -}}
{{- printf "%s-ssh" (include "jconfig.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "jconfig.pvcName" -}}
{{- default (include "jconfig.fullname" .) .Values.persistence.existingClaim -}}
{{- end -}}

{{/*
The rendered jconfig.yml. listen and management_listen come from the port
values so the probes, Service and container ports cannot drift apart from the
daemon's own configuration.
*/}}
{{- define "jconfig.config" -}}
listen: "0.0.0.0:{{ .Values.metricsPort }}"
{{- if .Values.management.enabled }}
management_listen: "127.0.0.1:{{ .Values.management.port }}"
{{- else }}
management_listen: ""
{{- end }}
metrics_path: /metrics
log_level: {{ .Values.config.logLevel | quote }}
log_format: {{ .Values.config.logFormat | quote }}

scheduler:
{{ toYaml .Values.config.scheduler | indent 2 }}

repo:
{{ toYaml .Values.config.repo | indent 2 }}

defaults:
{{ toYaml .Values.config.defaults | indent 2 }}

devices:
{{ toYaml .Values.config.devices | indent 2 }}
{{- end -}}
