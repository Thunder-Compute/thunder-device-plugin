{{- define "thunder-device-plugin.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "thunder-device-plugin.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "thunder-device-plugin.name" . -}}
{{- end -}}
{{- end -}}

{{- define "thunder-device-plugin.labels" -}}
app.kubernetes.io/name: {{ include "thunder-device-plugin.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "thunder-device-plugin.selectorLabels" -}}
app.kubernetes.io/name: {{ include "thunder-device-plugin.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: daemon
{{- end -}}

{{- define "thunder-device-plugin.operatorSelectorLabels" -}}
app.kubernetes.io/name: {{ include "thunder-device-plugin.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: operator
{{- end -}}


{{- define "thunder-device-plugin.secretName" -}}
{{- if .Values.thunder.existingSecret -}}
{{- .Values.thunder.existingSecret -}}
{{- else -}}
{{- .Values.thunder.secretName -}}
{{- end -}}
{{- end -}}
