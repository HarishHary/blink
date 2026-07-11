{{/* Merge per-log-type scaler overrides with chart defaults. */}}
{{- define "keda.logTypeConfig" -}}
{{- toYaml (mergeOverwrite (deepCopy .root.Values.logTypeDefaults) (.overrides | default dict)) -}}
{{- end }}

{{- define "keda.sharedStage" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $sharedStages := required "global.sharedStages is required; pass deployments/helm/values.yaml" (get $global "sharedStages") -}}
{{- toYaml (required (printf "global.sharedStages.%s is required" .stage) (get $sharedStages .stage)) -}}
{{- end }}

{{- define "keda.sharedStageConfig" -}}
{{- toYaml (mergeOverwrite (deepCopy (.root.Values.sharedStageDefaults | default dict)) (.overrides | default dict)) -}}
{{- end }}

{{- define "keda.stageTopic" -}}
{{- printf "%s-topic" . -}}
{{- end }}

{{- define "keda.stageGroup" -}}
{{- printf "%s-group" . -}}
{{- end }}

{{- define "keda.scalerName" -}}
{{- printf "%s-scaler" . -}}
{{- end }}

{{- define "keda.matcherName" -}}
{{- printf "event-matcher-%s" . -}}
{{- end }}

{{- define "keda.executorName" -}}
{{- printf "rule-executor-%s" . -}}
{{- end }}

{{- define "keda.matcherTopic" -}}
{{- printf "matcher-%s-topic" . -}}
{{- end }}

{{- define "keda.executorTopic" -}}
{{- printf "exec-%s-topic" . -}}
{{- end }}

{{- define "keda.matcherGroup" -}}
{{- printf "matcher-%s-group" . -}}
{{- end }}

{{- define "keda.executorGroup" -}}
{{- printf "exec-%s-group" . -}}
{{- end }}

{{- define "keda.matcherScalerName" -}}
{{- printf "event-matcher-%s-scaler" . -}}
{{- end }}

{{- define "keda.executorScalerName" -}}
{{- printf "rule-executor-%s-scaler" . -}}
{{- end }}
