{{/*
Standard chart name/label helpers, following the conventions from the
default Helm chart scaffold (`helm create`) so this chart behaves the way
anyone familiar with Helm would expect.
*/}}

{{- define "pgarchimigrator.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pgarchimigrator.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pgarchimigrator.labels" -}}
app.kubernetes.io/name: {{ include "pgarchimigrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "pgarchimigrator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pgarchimigrator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pgarchimigrator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pgarchimigrator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Resolves to the Secret name/key holding PGARCHIMIGRATOR_DATABASE_URL — either the
user-supplied existingSecret, or this chart's own auto-created Secret
(templates/secret.yaml) when database.inlineDSN was set instead.
*/}}
{{- define "pgarchimigrator.dbSecretName" -}}
{{- if .Values.database.existingSecret.name -}}
{{- .Values.database.existingSecret.name -}}
{{- else -}}
{{- include "pgarchimigrator.fullname" . -}}-db
{{- end -}}
{{- end -}}

{{- define "pgarchimigrator.dbSecretKey" -}}
{{- if .Values.database.existingSecret.name -}}
{{- .Values.database.existingSecret.key -}}
{{- else -}}
database-url
{{- end -}}
{{- end -}}
