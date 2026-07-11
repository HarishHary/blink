{{/* Merge per-log-type topic overrides with chart defaults. */}}
{{- define "kafka.logTypeConfig" -}}
{{- toYaml (mergeOverwrite (deepCopy .root.Values.logTypeDefaults) (.overrides | default dict)) -}}
{{- end }}

{{- define "kafka.sharedStage" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $sharedStages := required "global.sharedStages is required; pass deployments/helm/values.yaml" (get $global "sharedStages") -}}
{{- toYaml (required (printf "global.sharedStages.%s is required" .stage) (get $sharedStages .stage)) -}}
{{- end }}

{{- define "kafka.sharedStageConfig" -}}
{{- toYaml (mergeOverwrite (deepCopy (.root.Values.sharedStageDefaults | default dict)) (.overrides | default dict)) -}}
{{- end }}

{{- define "kafka.stageTopic" -}}
{{- printf "%s-topic" . -}}
{{- end }}

{{- define "kafka.stageDLQTopic" -}}
{{- printf "%s-dlq-topic" . -}}
{{- end }}

{{- define "kafka.snapshotTopic" -}}
{{- printf "%s-snapshot-topic" . -}}
{{- end }}

{{- define "kafka.matcherTopic" -}}
{{- printf "matcher-%s-topic" . -}}
{{- end }}

{{- define "kafka.executorTopic" -}}
{{- printf "exec-%s-topic" . -}}
{{- end }}
