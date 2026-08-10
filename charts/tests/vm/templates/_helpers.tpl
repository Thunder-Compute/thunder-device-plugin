{{- define "thunder-gpu-test-vm.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "thunder-gpu-test-vm.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "thunder-gpu-test-vm.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "thunder-gpu-test-vm.labels" -}}
app.kubernetes.io/name: {{ include "thunder-gpu-test-vm.name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}


{{- define "thunder-gpu-test-vm.rootDiskName" -}}
{{- if .Values.dataVolume.name -}}
{{- .Values.dataVolume.name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-rootdisk" (include "thunder-gpu-test-vm.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "thunder-gpu-test-vm.claimName" -}}
{{- printf "%s-claim" (include "thunder-gpu-test-vm.fullname" .) | trunc 253 | trimSuffix "-" -}}
{{- end -}}

{{- define "thunder-gpu-test-vm.guestConfigMapName" -}}
{{- printf "%s-thunder-configmap" (include "thunder-gpu-test-vm.claimName" .) | trunc 253 | trimSuffix "-" -}}
{{- end -}}

{{- define "thunder-gpu-test-vm.guestSecretName" -}}
{{- printf "%s-thunder-secret" (include "thunder-gpu-test-vm.claimName" .) | trunc 253 | trimSuffix "-" -}}
{{- end -}}
