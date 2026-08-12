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
app.kubernetes.io/version: {{ .Chart.Version | quote }}
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


{{/* The driver gets its own subdirectory under the kubelet plugin root, so the
socket path always matches the driver name the kubelet registered. */}}
{{- define "thunder-device-plugin.kubeletPluginDir" -}}
{{- printf "%s/%s" (trimSuffix "/" .Values.kubelet.pluginDirRoot) .Values.driverName -}}
{{- end -}}

{{/* Image reference for one component. values.yaml records the released image
tag, while a digest takes precedence when one is set. */}}
{{- define "thunder-device-plugin.image" -}}
{{- $image := .image -}}
{{- if $image.digest -}}
{{- printf "%s@%s" $image.repository $image.digest -}}
{{- else -}}
{{- printf "%s:%s" $image.repository $image.tag -}}
{{- end -}}
{{- end -}}
