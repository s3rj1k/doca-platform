{{/*
Expand the name of the chart.
*/}}
{{- define "dpf-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "dpf-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "dpf-operator.labels" -}}
helm.sh/chart: {{ include "dpf-operator.chart" . }}
{{ include "dpf-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "dpf-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dpf-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Validate and return the cluster manager. Only absent or empty falls back, so a value computed
to false reaches the check rather than being read as unset.
*/}}
{{- define "dpf-operator.clusterManager" -}}
{{- $clusterManager := .Values.clusterManager -}}
{{- if or (kindIs "invalid" $clusterManager) (eq (toString $clusterManager) "") -}}
{{- $clusterManager = "kamaji" -}}
{{- end -}}
{{- if not (has $clusterManager (list "kamaji" "static")) -}}
{{- fail (printf "clusterManager must be either \"kamaji\" or \"static\", got %v" .Values.clusterManager) -}}
{{- end -}}
{{- $clusterManager -}}
{{- end }}
