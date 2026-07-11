{{/*
Standard labels applied to every resource.
*/}}
{{- define "blink.labels" -}}
app.kubernetes.io/part-of: blink
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
envFrom block - every pod sources shared config and auth.
*/}}
{{- define "blink.envFrom" -}}
- configMapRef:
    name: blink-config
- secretRef:
    name: blink-auth
{{- end }}

{{/*
Standard HTTP health probes on port 8080.
*/}}
{{- define "blink.probes" -}}
readinessProbe:
  httpGet:
    path: /health/ready
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 10
livenessProbe:
  httpGet:
    path: /health/live
    port: 8080
  initialDelaySeconds: 10
  periodSeconds: 20
{{- end }}

{{/*
Plugin volume definition - hostPath by default, swap for PVC in production.
Call with root context: include "blink.pluginVolume" $
*/}}
{{- define "blink.pluginVolume" -}}
- name: plugins
  {{- if .Values.plugins.volume.hostPath }}
  hostPath:
    path: {{ .Values.plugins.volume.hostPath.path }}
    type: {{ .Values.plugins.volume.hostPath.type }}
  {{- else if .Values.plugins.volume.persistentVolumeClaim }}
  persistentVolumeClaim:
    claimName: {{ .Values.plugins.volume.persistentVolumeClaim.claimName }}
  {{- else }}
  emptyDir: {}
  {{- end }}
{{- end }}

{{/*
Plugin volume mount - always at /plugins.
*/}}
{{- define "blink.pluginVolumeMount" -}}
- name: plugins
  mountPath: /plugins
{{- end }}

{{/*
Image reference: registry/name:tag
Args: dict "registry" $registry "name" $name "tag" $tag
*/}}
{{- define "blink.image" -}}
{{ .registry }}/{{ .name }}:{{ .tag }}
{{- end }}

{{/* Merge per-log-type overrides with chart defaults. */}}
{{- define "blink.logTypeConfig" -}}
{{- toYaml (mergeOverwrite (deepCopy .root.Values.logTypeDefaults) (.overrides | default dict)) -}}
{{- end }}

{{/* Return the shared-stage topology declared in the common values file. */}}
{{- define "blink.sharedStage" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $sharedStages := required "global.sharedStages is required; pass deployments/helm/values.yaml" (get $global "sharedStages") -}}
{{- toYaml (required (printf "global.sharedStages.%s is required" .stage) (get $sharedStages .stage)) -}}
{{- end }}

{{/* Merge shared-stage capacity overrides with this chart's defaults. */}}
{{- define "blink.sharedStageConfig" -}}
{{- toYaml (mergeOverwrite (deepCopy (.root.Values.sharedStageDefaults | default dict)) (.overrides | default dict)) -}}
{{- end }}

{{- define "blink.stageTopic" -}}
{{- printf "%s-topic" . -}}
{{- end }}

{{- define "blink.stageGroup" -}}
{{- printf "%s-group" . -}}
{{- end }}

{{- define "blink.stageDLQTopic" -}}
{{- printf "%s-dlq-topic" . -}}
{{- end }}

{{- define "blink.snapshotTopic" -}}
{{- printf "%s-snapshot-topic" . -}}
{{- end }}

{{- define "blink.matcherName" -}}
{{- printf "event-matcher-%s" . -}}
{{- end }}

{{- define "blink.executorName" -}}
{{- printf "rule-executor-%s" . -}}
{{- end }}

{{- define "blink.matcherTopic" -}}
{{- printf "matcher-%s-topic" . -}}
{{- end }}

{{- define "blink.executorTopic" -}}
{{- printf "exec-%s-topic" . -}}
{{- end }}

{{- define "blink.matcherGroup" -}}
{{- printf "matcher-%s-group" . -}}
{{- end }}

{{- define "blink.executorGroup" -}}
{{- printf "exec-%s-group" . -}}
{{- end }}
