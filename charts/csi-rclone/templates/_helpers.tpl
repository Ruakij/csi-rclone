{{- define "csi-rclone.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "csi-rclone.fullname" -}}
{{- if contains (include "csi-rclone.name" .) .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "csi-rclone.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "csi-rclone.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "csi-rclone.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "csi-rclone.selectorLabels" -}}
app.kubernetes.io/name: {{ include "csi-rclone.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "csi-rclone.controllerServiceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (printf "%s-controller" (include "csi-rclone.fullname" .)) .Values.serviceAccount.controllerName -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.controllerName -}}
{{- end -}}
{{- end -}}

{{- define "csi-rclone.nodeServiceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (printf "%s-node" (include "csi-rclone.fullname" .)) .Values.serviceAccount.nodeName -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.nodeName -}}
{{- end -}}
{{- end -}}

{{- define "csi-rclone.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{- define "csi-rclone.kubeletRootDir" -}}
{{- trimSuffix "/" .Values.kubeletRootDir -}}
{{- end -}}
