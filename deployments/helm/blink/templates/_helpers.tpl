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

{{/* Return stage topology declared in the common values file. */}}
{{- define "blink.stage" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $stages := required "global.stages is required; pass deployments/helm/values.yaml" (get $global "stages") -}}
{{- $stage := deepCopy (required (printf "global.stages.%s is required" .stage) (get $stages .stage)) -}}
{{- $workload := required (printf "global.stages.%s.workload is required" .stage) (get $stage "workload") -}}
{{- $workloadName := required (printf "global.stages.%s.workload.name is required" .stage) (get $workload "name") -}}
{{- toYaml (mergeOverwrite $stage (dict "workloadName" $workloadName)) -}}
{{- end }}

{{/* Merge a stage's workload settings with this chart's defaults. */}}
{{- define "blink.workloadConfig" -}}
{{- toYaml (mergeOverwrite (deepCopy (.root.Values.workloadDefaults | default dict)) (.overrides | default dict)) -}}
{{- end }}
