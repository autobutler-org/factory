{{/* Name of the server's resources: the release name, or release-chart when the release name lacks the chart name. */}}
{{- define "factory.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "factory.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "factory.labels" -}}
{{ include "factory.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Labels for chart objects in the worker namespace. Not the worker pods' labels: the server sets those. */}}
{{- define "factory.workerLabels" -}}
app.kubernetes.io/part-of: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* repository[:tag][@digest] from a dict with repository, tag, digest. */}}
{{- define "factory.imageRef" -}}
{{- .repository -}}
{{- with .tag }}:{{ . }}{{ end -}}
{{- with .digest }}@{{ . }}{{ end -}}
{{- end -}}

{{- define "factory.image" -}}
{{- include "factory.imageRef" (dict "repository" .Values.image.repository "tag" (.Values.image.tag | default .Chart.AppVersion) "digest" .Values.image.digest) -}}
{{- end -}}

{{- define "factory.workerImage" -}}
{{- $tag := .Values.workers.image.tag | default .Values.image.tag | default .Chart.AppVersion -}}
{{- include "factory.imageRef" (dict "repository" .Values.workers.image.repository "tag" $tag "digest" .Values.workers.image.digest) -}}
{{- end -}}

{{/* A map rendered as key=value,key=value, keys sorted. */}}
{{- define "factory.pairs" -}}
{{- $pairs := list -}}
{{- range $k, $v := . -}}
{{- $pairs = append $pairs (printf "%s=%v" $k $v) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end -}}

{{/* Tolerations rendered as key=value:Effect (Equal) or key:Effect (Exists). */}}
{{- define "factory.tolerations" -}}
{{- $items := list -}}
{{- range . -}}
{{- $key := required "launcher.tolerations: each entry needs a key" .key -}}
{{- $effect := required "launcher.tolerations: each entry needs an effect, for example NoSchedule" .effect -}}
{{- if .value -}}
{{- $items = append $items (printf "%s=%v:%s" $key .value $effect) -}}
{{- else -}}
{{- $items = append $items (printf "%s:%s" $key $effect) -}}
{{- end -}}
{{- end -}}
{{- join "," $items -}}
{{- end -}}

{{- define "factory.serverUrl" -}}
{{- .Values.launcher.serverUrl | default (printf "http://%s.%s.svc.cluster.local:%v" (include "factory.fullname" .) .Release.Namespace .Values.service.port) -}}
{{- end -}}

{{/* Pod and container securityContext that meet the Pod Security restricted level. */}}
{{- define "factory.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "factory.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop:
    - ALL
{{- end -}}

{{- define "factory.databaseEnv" -}}
- name: FACTORY_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.secretName }}
      key: {{ .Values.database.secretKey }}
{{- end -}}

{{/* The in-cluster Postgres pod's labels. Distinct from factory.selectorLabels so the server's Service and
Deployment never select it. */}}
{{- define "factory.postgresSelectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}-postgres
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
