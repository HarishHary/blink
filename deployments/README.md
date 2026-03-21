# Blink Kubernetes Deployment

## Prerequisites

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

### Kafka operators

```bash
kubectl create namespace kafka
kubectl create -f 'https://strimzi.io/install/latest?namespace=kafka' -n kafka
kubectl get pod -n kafka --watch => wait to be ready!
```

### KEDA operators

```bash
helm repo add kedacore https://kedacore.github.io/charts
helm repo update
helm install keda kedacore/keda --namespace keda --create-namespace
kubectl get pods -n keda --watch => wait to be ready!
```

## Apply order

```bash
# 1. Namespace
kubectl apply -f kafka/namespace.yaml
kubectl apply -f blink/namespace.yaml

# 2. Kafka cluster - wait for Ready before applying topics (~2 min)
kubectl apply -f kafka/cluster.yaml
kubectl wait kafka/blink-kafka-cluster --for=condition=Ready --timeout=300s -n kafka

# 3. Kafka topics
kubectl apply -f kafka/topics.yaml

# 4. ConfigMap + Secret
kubectl apply -f blink/common-config.yaml

# 5. Services - all stages are fully Kafka-wired
kubectl apply -f blink/event-matcher-deployment.yaml
kubectl apply -f blink/rule-executor-application.yaml
kubectl apply -f blink/rule-executor-auth.yaml

# 6. KEDA ScaledObjects (after services are running)
# FIXME
```

## Service Image

* Since we are using minikube. We need to make the images available ! Send your locally built images inside the cluster.

```bash
docker build -t blink-event-matcher:latest -f cmd/event_matcher/Dockerfile . --output type=docker
minikube image load blink-event-matcher:latest
```

## Pipeline flow

```text
blink-matcher-*  =>  event_matcher   =>  blink-exec
blink-exec       =>  rule_executor   =>  blink-merger
blink-merger     =>  alert_merger    =>  blink-tuner
blink-tuner      =>  rule_tuner      =>  blink-enricher
blink-enricher   =>  alert_enricher  =>  blink-formatter
blink-formatter  =>  alert_formatter =>  blink-dispatcher
blink-dispatcher =>  alert_dispatcher
```

## Plugin binaries

Each service watches its plugin directory via fsnotify. The `emptyDir` volumes in
the Deployments mean no plugins are loaded at startup (services pass events through).
To load plugins, replace `emptyDir` with a `PersistentVolumeClaim` pre-populated
with plugin binaries, or use an `initContainer` to copy them from a registry image.

* Mount the plugin local folder in minikube

```bash
minikube mount ~/.blink/plugins:/blink/plugins
```

* All the deployments uses the HostPath to mount the plugin folder.

* Compile and write the plugin in that local folder

```bash
GOOS=linux GOARCH=arm64 go build -o ~/.blink/plugins/matchers/allow-all ./examples/matchers/allow-all/
```
