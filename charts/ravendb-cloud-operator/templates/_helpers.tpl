{{- define "operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "operator.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: controller-manager
{{- end -}}

{{- define "operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
A namespace-scoped Role only makes sense when the operator is also restricted
to that namespace. Asking for one without watchNamespace would install an
operator that watches everywhere and may act nowhere, which fails at runtime
with permission errors rather than at install time. So it fails here.
*/}}
{{- define "operator.validateScope" -}}
{{- if and (eq .Values.rbac.scope "namespace") (not .Values.watchNamespace) -}}
{{- fail "rbac.scope=namespace requires watchNamespace to be set, or the operator would watch namespaces it cannot read" -}}
{{- end -}}
{{- if not (has .Values.rbac.scope (list "" "cluster" "namespace")) -}}
{{- fail (printf "rbac.scope must be empty, cluster or namespace, got %q" .Values.rbac.scope) -}}
{{- end -}}
{{- end -}}

{{/*
operator.rbacScope is rbac.scope, or, when that is empty, what watchNamespace
implies: an operator kept to one namespace needs permissions in that one only.
*/}}
{{- define "operator.rbacScope" -}}
{{- if .Values.rbac.scope -}}
{{- .Values.rbac.scope -}}
{{- else if .Values.watchNamespace -}}
namespace
{{- else -}}
cluster
{{- end -}}
{{- end -}}
