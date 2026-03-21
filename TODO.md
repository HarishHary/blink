// rule engine service
Match(event events.Event) (bool, errors.Error)
Evaluate(event events.Event) (bool, errors.Error)

// need to have process malfunctions between rule engine and alert engine

// alert engine service
Enrich(alert *alerts.Alert) errors.Error // can have blocking functions => implement retry logic, and try later queue
Tune(alert alerts.Alert) (bool, errors.Error)

// need to have process malfunctions between alert engine and alert processor

// alert processor service
Format(alert alerts.Alert) (map[string]any, errors.Error) // done by the processor service
Dispatch(alert alerts.Alert) (bool, errors.Error) // done by the processor service => implement retry logic, and try later queue

// need to add dosc asdqwe
// need to add tests
// need to add CI/CD
// need to add a way to test a rule with the local rule engine
// need to add a way to test an alert with the local alert engine
// need to add a way to test an alert with the local alert processor
// need to implement asset tagging
// need to implement global tuning rules
// need to implement VRL service with VRL rules
// need to implement signal type rules and correlation rules
// need to implement graph based rule engine with FalconHound
// need to implement UI
// need a way to load rules from metadata in yaml + files

## Performance:

    * **Pre‑compile your rules** (e.g. regex, expression trees) at startup so rule matching per event is just executing an in‑memory function rather than parsing on every message.
    * **Partition by rule‑group**: if you have 10 K rules, shard them into N groups so each instance only holds ~1 K rules.
    * **Batch matching** (if possible): consume small batches of events and run them through your rule‑engine in parallel goroutines, reusing the compiled rule set in memory.

    Enrichment and tuning often require calling external systems (databases, ML models, HTTP APIs).  Those calls:
      * Vary hugely in latency (50 ms for a simple lookup vs seconds/minutes for a model inference)
      * May need caching

A quick pattern: pull only the events/alerts you need to enrich from your broker, fan‑out enrichment requests to a worker pool with concurrency limits, and publish back a “enriched alert”
message when done.  Failures go to a DLQ.  You can scale this layer up (more pods, bigger instance types) independently to match the slowest external call.

## Autoscaling & resource sizing

On Kubernetes:

    * **Horizontal Pod Autoscaler** (HPA) on CPU, memory, or custom metrics (e.g. queue length).
    * **Vertical Pod Autoscaler** (VPA) for CPU/memory tuning.
    * **Pod Disruption Budgets** to maintain availability.

Instrument your services (Prometheus + client‑go) to expose metrics like:

    * input rate
    * processing latency
    * error rate / DLQ rate
    * queue depth

Then hook those metrics into your HPA.

## Observability & tracing

At cloud scale you can’t diagnose manually.  Add:

    * **Structured logging** (with request‑ID, trace‑ID)
    * **Distributed tracing** (OpenTelemetry spans across microservices)
    * **Dashboards/alerts** on error spikes, DLQ growth, latency SLO breaches

This gives you a real‑time view of how many events/rules/enrichments are in flight.