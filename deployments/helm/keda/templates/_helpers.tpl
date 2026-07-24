{{- define "keda.stage" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $stages := required "global.stages is required; pass deployments/helm/values.yaml" (get $global "stages") -}}
{{- $stage := deepCopy (required (printf "global.stages.%s is required" .stage) (get $stages .stage)) -}}
{{- $workload := required (printf "global.stages.%s.workload is required" .stage) (get $stage "workload") -}}
{{- $workloadName := required (printf "global.stages.%s.workload.name is required" .stage) (get $workload "name") -}}
{{- $environment := get $workload "environment" | default dict -}}
{{- $envName := .stage | replace "-" "_" -}}
{{- $topicName := required (printf "global.stages.%s.workload.environment.kafka_topic_%s is required" .stage $envName) (get $environment (printf "kafka_topic_%s" $envName)) -}}
{{- $consumerGroup := required (printf "global.stages.%s.workload.environment.kafka_group_%s is required" .stage $envName) (get $environment (printf "kafka_group_%s" $envName)) -}}
{{- toYaml (mergeOverwrite $stage (dict "workloadName" $workloadName "topicName" $topicName "consumerGroup" $consumerGroup)) -}}
{{- end }}

{{- define "keda.scalerConfig" -}}
{{- toYaml (mergeOverwrite (deepCopy (.root.Values.scalerDefaults | default dict)) (.overrides | default dict)) -}}
{{- end }}
