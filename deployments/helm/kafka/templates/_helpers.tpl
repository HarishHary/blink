{{- define "kafka.stage" -}}
{{- $global := .root.Values.global | default dict -}}
{{- $stages := required "global.stages is required; pass deployments/helm/values.yaml" (get $global "stages") -}}
{{- $stage := deepCopy (required (printf "global.stages.%s is required" .stage) (get $stages .stage)) -}}
{{- $workload := required (printf "global.stages.%s.workload is required" .stage) (get $stage "workload") -}}
{{- $workloadName := required (printf "global.stages.%s.workload.name is required" .stage) (get $workload "name") -}}
{{- $environment := get $workload "environment" | default dict -}}
{{- $envName := .stage | replace "-" "_" -}}
{{- $topic := get $stage "topic" | default dict -}}
{{- $topicName := required (printf "global.stages.%s.workload.environment.kafka_topic_%s is required" .stage $envName) (get $environment (printf "kafka_topic_%s" $envName)) -}}
{{- $consumerGroup := required (printf "global.stages.%s.workload.environment.kafka_group_%s is required" .stage $envName) (get $environment (printf "kafka_group_%s" $envName)) -}}
{{- $dlqTopicName := "" -}}
{{- if hasKey $topic "dlq" -}}
{{- $dlqTopicName = required (printf "global.stages.%s.workload.environment.kafka_topic_%s_dlq is required" .stage $envName) (get $environment (printf "kafka_topic_%s_dlq" $envName)) -}}
{{- end -}}
{{- $snapshotTopicName := "" -}}
{{- if hasKey $topic "snapshot" -}}
{{- $snapshotTopicName = required (printf "global.stages.%s.workload.environment.kafka_topic_%s_snapshot is required" .stage $envName) (get $environment (printf "kafka_topic_%s_snapshot" $envName)) -}}
{{- end -}}
{{- toYaml (mergeOverwrite $stage (dict "workloadName" $workloadName "topicName" $topicName "consumerGroup" $consumerGroup "dlqTopicName" $dlqTopicName "withDLQ" (hasKey $topic "dlq") "snapshotTopicName" $snapshotTopicName "withSnapshot" (hasKey $topic "snapshot"))) -}}
{{- end }}

{{- define "kafka.topic" -}}
{{- toYaml (mergeOverwrite (deepCopy (.root.Values.topicDefaults | default dict)) (.overrides | default dict)) -}}
{{- end }}
