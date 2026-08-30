# Blink Kubernetes deployment

[Services](../docs/services/README.md) · [Runtime overview](../docs/internals/README.md)

Runbook for installing and operating Blink on Kubernetes: prerequisites, the Helm values contract, and the commands to verify and repair a running deployment. Runtime behavior belongs to the service docs.

## Local prerequisites

### Podman

```bash
brew install podman
podman system connection default podman-machine-default-root
podman machine init --cpus 5 --memory 16384 --disk-size 100
podman machine set --rootful
podman machine start
```

### Minikube

```bash
minikube config set cpus 2
minikube config set memory 4096
minikube config set disk-size 40
minikube config set rootless false
minikube config set container-runtime crio
minikube config set driver podman
minikube start
```

### Kafka

```bash
kubectl create namespace kafka
kubectl create -f 'https://strimzi.io/install/latest?namespace=kafka' -n kafka
kubectl get pods -n kafka --watch # wait for them to be ready
```

### KEDA

```bash
helm repo add kedacore https://kedacore.github.io/charts
helm repo update
helm install keda kedacore/keda --namespace keda --create-namespace
kubectl get pods -n keda --watch # wait for them to be ready
```

### etcd

Unlike Kafka and KEDA, etcd is not an external operator to install first - `deployments/helm/blink/templates/etcd.yaml` deploys a dedicated 3-member etcd cluster as part of the Blink chart itself, for Blink's exclusive use as its Ergo cluster registrar (every workload, including the controller, requires it; see `docs/internals/controller-runtime.md`). The only prerequisite here is setting the values that authenticate against it before installing the Blink chart:

```bash
# cluster.cookie authenticates node-to-node connections; etcd.username/etcd.password authenticate
# against etcd itself. All three default to "" and must be overridden for any shared environment.
helm upgrade --install blink deployments/helm/blink --namespace blink --create-namespace \
  -f deployments/helm/values.yaml \
  --set cluster.cookie="$CLUSTER_COOKIE" \
  --set etcd.username="$ETCD_USERNAME" \
  --set etcd.password="$ETCD_PASSWORD"
```

### Build images

For Minikube, build the two documented images into its image store:

```bash
minikube image build --tag localhost/blink-controller:latest --file cmd/controller/Dockerfile .
minikube image build --tag localhost/blink-event-matcher:latest --file cmd/event_matcher/Dockerfile .
```

## Helm validation and install

Every chart must receive the same topology file:

```bash
# Validate and render every chart from the repository root.
helm lint deployments/helm/kafka -f deployments/helm/values.yaml
helm template blink-kafka deployments/helm/kafka --namespace kafka -f deployments/helm/values.yaml
helm lint deployments/helm/blink -f deployments/helm/values.yaml
helm template blink deployments/helm/blink --namespace blink -f deployments/helm/values.yaml
helm lint deployments/helm/keda -f deployments/helm/values.yaml
helm template blink-keda deployments/helm/keda --namespace blink -f deployments/helm/values.yaml

# Install in dependency order. The Blink chart requires Kafka topics and the KEDA
# chart targets Deployments created by the Blink chart.
helm upgrade --install blink-kafka deployments/helm/kafka --namespace kafka --create-namespace -f deployments/helm/values.yaml
kubectl wait kafka/blink-kafka-cluster --for=condition=Ready --timeout=300s --namespace kafka
helm upgrade --install blink deployments/helm/blink --namespace blink --create-namespace -f deployments/helm/values.yaml
helm upgrade --install blink-keda deployments/helm/keda --namespace blink -f deployments/helm/values.yaml
```

The Blink chart defaults to `localhost`, `latest`, and `image.pullPolicy: Never`. Override image settings for a registry-backed cluster.

## Workload topology

`workloads.yaml` renders one Deployment per `global.stages` key plus `global.controller`, all from the same template. Adding a stage adds a workload; no template edit.

| Workload           | Values key          | Plugin volume | Ergo node | Notes                                                                     |
| ------------------ | ------------------- | ------------- | --------- | ------------------------------------------------------------------------- |
| `blink-controller` | `global.controller` | yes           | yes       | One replica only; mounts controller state at `/var/lib/blink/controller`. |
| `event-matcher`    | `stages.matcher`    | yes           | yes       |                                                                           |
| `rule-executor`    | `stages.executor`   | yes           | yes       |                                                                           |
| `alert-merger`     | `stages.merger`     | no            | no        | Runs no Ergo node, so both are set `false`.                               |
| `rule-tuner`       | `stages.tuner`      | yes           | yes       |                                                                           |
| `alert-enricher`   | `stages.enricher`   | yes           | yes       |                                                                           |
| `alert-formatter`  | `stages.formatter`  | yes           | yes       |                                                                           |
| `alert-dispatcher` | `stages.dispatcher` | yes           | yes       | No `cmd/` binary builds this image yet.                                   |

Each `workload` map carries `name`, `container`, `replicas`, `image`, `resources`, and `environment`. `plugins`, `node`, and `radar` default to `true`; `observer` and `mcp` to `false`. Snake-case keys in `environment` render as uppercase environment variables, so a stage's topics, consumer group, DLQ, and plugin directory are all configured there.

Three constraints the template enforces, failing the render rather than deploying something broken:

- the controller at any replica count other than 1, until publication leader election exists;
- `controllerState` with no PVC and no `controllerState.existingClaim`;
- any of `radar`, `observer`, or `mcp` on a workload with `node: false`, which runs no node to serve them.

Every workload running an Ergo node joins one cluster: `etcd.yaml` deploys a dedicated 3-member etcd for node discovery, and `cluster.cookie` authenticates node-to-node connections. Both are Blink's own, not shared infrastructure. This is the only path for snapshot distribution - no Kafka topic is involved.

The plugin volume is the host path `/blink/plugins` locally, or `plugins.volume.persistentVolumeClaim`. The controller needs all five catalog directories; each executor needs only its own.

What actually flows between these workloads at runtime is in [services](../docs/services/README.md) and [message flow](../docs/internals/message-flow.md).

## Verify

```bash
kubectl get deployments,pods,services -n blink
kubectl get kafka,kafkatopic -n kafka
kubectl get scaledobjects -n blink
kubectl rollout status deployment/event-matcher --namespace blink --timeout=120s
```

Every workload serves `/health/live`, `/health/ready`, and `/metrics` on port 8080. Readiness is per service - see [services](../docs/services/README.md); `event-matcher` for instance waits on both projections before it consumes.

Every workload that runs an Ergo node additionally serves radar on port 9090 (`radar.port`, `RADAR_HOST`/`RADAR_PORT`): `/metrics` for its per-namespace control-plane series and `/health/ready`,
which reports 503 while any namespace on that node is not ready. Alert merger runs no node and sets `node: false`, so it has neither the port nor the scrape annotation. The kubelet probes stay on 8080 on purpose - the controller Service is how executors resolve the Ergo cluster port, so a degraded namespace must not remove the pod from it.

```bash
kubectl port-forward deployment/blink-controller 9090:9090 --namespace blink
curl -s localhost:9090/health/ready | jq .
curl -s localhost:9090/metrics | grep blink_controller_availability

kubectl port-forward deployment/rule-executor 9090:9090 --namespace blink
curl -s localhost:9090/metrics | grep blink_plugin_
```

### Observer and MCP

Ergo's observer UI (9911, `observer.port`) inspects a live node's processes, supervision tree, and mailboxes; its MCP server (9922, `mcp.port`) exposes the same to an agent. Both are off everywhere by default and enabled the same way radar is - `observer: true` or `mcp: true` on one workload adds that port, its Service entry, and its `OBSERVER_*`/`MCP_*` variables. Nothing about `environment` changes this.

```bash
helm upgrade --install blink deployments/helm/blink --namespace blink -f deployments/helm/values.yaml \
  --set global.stages.matcher.workload.observer=true
kubectl port-forward deployment/event-matcher 9911:9911 --namespace blink
open http://localhost:9911
```

Leave both off in shared clusters: neither is authenticated, and both can send messages to live processes.

Debug logging is chart-wide, not per workload: `--set debug=true` puts `DEBUG=true` in `blink-config`, raising both Blink's logger and every Ergo node to debug level.

## Monitoring

`deployments/helm/blink/templates/monitoring.yaml` installs Grafana and the Prometheus that feeds it as part of the Blink chart, like etcd. Prometheus discovers pods by annotation, so no target lists need maintaining: every workload is scraped on 8080 (runner restarts, Go and process metrics) and every pod carrying the `blink.io/radar-port` annotation is scraped a second time on its radar port (every `blink_controller_*`, `blink_plugin_*`, and `blink_snapshot_*` series).
Grafana provisions the datasource and three dashboards from `deployments/helm/blink/files/grafana/dashboards/`, so a fresh install already has them in the `Blink` folder: **Blink Controller**, **Blink Plugin Runtime**, and **Blink Snapshot Runtime**. Between them they panel every `blink_*` series the runtime publishes.

```bash
kubectl port-forward deployment/blink-grafana 3000:3000 --namespace blink
open http://localhost:3000   # admin/admin by default; --set monitoring.grafana.adminPassword=...
```

Each dashboard is grouped the same way - availability and lifecycle state first, then the flow, then the layer detail behind a degraded namespace - and each is filtered by its own namespace variable.

- **Blink Controller** - commit flow, then writer queue and database attempts, artifact files the scanner could not index by stage, writer I/O fences, controller-actor terminations by reason, and
  application load/close outcomes per runner attempt.
- **Blink Plugin Runtime** - the rollout transition and the revisions and generations behind it, then routers and plugin processes, invocation rate and latency, every way a call is rejected before a
  plugin sees it, and the router, process, and child churn under a live supervisor.
- **Blink Snapshot Runtime** - reader, projection, and reported availability, then delivered vs serving generations and the lag the controller reads as drift, subscription attempts and controller
  losses, ignored updates by reason, parse results and latency, and external commit outcomes.

```bash
# Confirm both scrape jobs are up before blaming an empty panel.
kubectl port-forward deployment/blink-prometheus 9091:9090 --namespace blink
curl -s localhost:9091/api/v1/targets | jq '.data.activeTargets[] | {job: .labels.job, pod: .labels.pod, health}'
```

| Value                                                                               | Effect                                                                                                                                         |
| ----------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `monitoring.enabled=false`                                                          | Installs neither. The pod annotations stay, so an existing cluster Prometheus still finds them.                                                |
| `monitoring.prometheus.enabled=false` with `monitoring.grafana.datasourceURL=<url>` | Grafana and the dashboard only, pointed at a Prometheus you already run.                                                                       |
| `monitoring.prometheus.persistence.enabled=true`                                    | Keeps metric history across pod restarts; off by default, since this Prometheus is for looking at a running deployment, not long-term storage. |
| `monitoring.grafana.service.type=NodePort` with `nodePort`                          | Reaches Grafana without a port-forward on Minikube.                                                                                            |
| `monitoring.grafana.anonymous=true`                                                 | Unauthenticated Viewer access - convenient locally, not for a shared cluster.                                                                  |

## Operational commands

These assume the `blink` (workloads) and `kafka` (broker) namespaces from this runbook. Substitute
your own namespaces if you installed elsewhere.

### Restart one service

Picks up a new image tag or an env var change without touching anything else:

```bash
kubectl rollout restart deployment/event-matcher --namespace blink
kubectl rollout status deployment/event-matcher --namespace blink --timeout=60s
```

### Rebuild and redeploy one service

`imagePullPolicy: Never` plus reusing the `:latest` tag means Kubernetes can skip pulling your
rebuilt image - tag every rebuild uniquely so the Deployment is guaranteed to pick it up:

```bash
TAG="dev-$(date +%s)"
minikube image build -t "localhost/blink-event-matcher:$TAG" -f cmd/event_matcher/Dockerfile .
kubectl set image deployment/event-matcher -n blink event-matcher="localhost/blink-event-matcher:$TAG"
kubectl rollout status deployment/event-matcher -n blink --timeout=60s
```

### Send test events

Produce newline-delimited JSON directly into a topic. `kafka-console-producer.sh` runs inside the
broker pod, so copy the file in first if it's larger than the broker's `/tmp` (often a few MB tmpfs

- copy to `/home/kafka` instead for anything sizable):

```bash
BROKER=$(kubectl get pods -n kafka -l app.kubernetes.io/name=kafka -o jsonpath='{.items[0].metadata.name}')
kubectl cp events.jsonl kafka/"$BROKER":/home/kafka/events.jsonl
kubectl exec -n kafka "$BROKER" -- bash -c \
  "bin/kafka-console-producer.sh --bootstrap-server localhost:9092 --topic event-matcher-topic < /home/kafka/events.jsonl"
```

### Restart one topic (empty it)

Strimzi has no "truncate" operation - delete and let the Topic Operator recreate it from its
`KafkaTopic` CR. A client that queries the topic name before the operator finishes recreating it
can trigger Kafka's own topic auto-create with the wrong (default 1) partition count, which the
operator then silently adopts as correct; wait a few seconds and confirm the partition count before
sending anything:

```bash
BROKER=$(kubectl get pods -n kafka -l app.kubernetes.io/name=kafka -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n kafka "$BROKER" -- bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --delete --topic event-matcher-topic
sleep 15
kubectl exec -n kafka "$BROKER" -- bin/kafka-topics.sh --bootstrap-server localhost:9092 \
  --describe --topic event-matcher-topic | head -1   # confirm PartitionCount matches the KafkaTopic CR
```

### Clean up (empty every pipeline topic)

For a clean load-test slate. Restart every consuming service afterward so it rejoins each
topic's consumer group with a fresh assignment - a consumer that was mid-session during the delete
can otherwise get stuck holding a stale partition assignment:

```bash
BROKER=$(kubectl get pods -n kafka -l app.kubernetes.io/name=kafka -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n kafka "$BROKER" -- bin/kafka-topics.sh --bootstrap-server localhost:9092 --delete --topic \
  'event-matcher-topic,event-matcher-dlq-topic,rule-executor-topic,alert-merger-topic,alert-merger-dlq-topic,rule-tuner-topic,rule-tuner-dlq-topic,alert-enricher-topic,alert-enricher-dlq-topic,alert-formatter-topic,alert-formatter-dlq-topic,alert-dispatcher-topic'
sleep 15
kubectl rollout restart deployment/event-matcher --namespace blink
kubectl rollout status deployment/event-matcher --namespace blink --timeout=60s
```

## Documents

- [Services index](../docs/services/README.md)
- [Controller](../docs/services/controller.md)
- [Event matcher](../docs/services/event_matcher.md)
- [Runtime overview](../docs/internals/README.md)
- [Message flow](../docs/internals/message-flow.md)
- [Concurrency knobs](../docs/internals/concurrency-knobs.md)
