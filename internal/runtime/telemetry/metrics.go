// Package telemetry is the radar plumbing every runtime layer shares: collector specs, one subject's
// bound label values, and a readiness signal. Metric names and their specs stay with the layer that
// publishes them.
package telemetry

import (
	"errors"
	"fmt"
	"time"

	"ergo.services/actor/metrics"
	"ergo.services/ergo/gen"
	"github.com/harishhary/blink/internal/runtime"
)

// Radar's own process names, unexported by the radar package, addressed directly by every layer here.
const (
	HealthProcess  = gen.Atom("radar_health")
	MetricsProcess = gen.Atom("radar_metrics")
)

type Kind int

const (
	Gauge Kind = iota
	Counter
	Histogram
)

// MetricSpec is one collector to create on radar; Buckets applies to a histogram only.
type MetricSpec struct {
	Kind    Kind
	Name    string
	Help    string
	Labels  []string
	Buckets []float64
}

// Registrar is the Call half of a process, which is all creating a collector needs.
type Registrar interface {
	Call(to any, request any) (any, error)
}

// Sender is the Send half of a process; gen.Process, gen.MetaProcess, and gen.Node all satisfy it.
type Sender interface {
	Send(to any, message any) error
}

var metricTypes = map[Kind]metrics.MetricType{
	Gauge:     metrics.MetricGauge,
	Counter:   metrics.MetricCounter,
	Histogram: metrics.MetricHistogram,
}

// Register creates every collector. Always pass the node: radar deletes a dead registrant's metrics.
func Register(registrar Registrar, specs []MetricSpec) error {
	if registrar == nil {
		return errors.New("radar metrics: no registrar")
	}
	for _, spec := range specs {
		if err := spec.register(registrar); err != nil {
			return err
		}
	}
	return nil
}

// register creates the metric on radar; a repeat registration of the same type and labels is a no-op.
func (s MetricSpec) register(registrar Registrar) error {
	result, err := registrar.Call(MetricsProcess, metrics.RegisterRequest{
		Name:    s.Name,
		Help:    s.Help,
		Type:    metricTypes[s.Kind],
		Labels:  s.Labels,
		Buckets: s.Buckets,
	})
	if err != nil {
		return err
	}
	response, ok := result.(metrics.RegisterResponse)
	if !ok {
		return fmt.Errorf("radar metrics: unexpected response %T", result)
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

// Labels are the leading label values every sample from one subject carries; they hold no registration
// state, so any layer can copy them.
type Labels struct{ values []string }

// NewLabels fixes the leading label values every metric published through them carries.
func NewLabels(values ...string) Labels {
	return Labels{values: values}
}

// Set publishes a gauge value.
func (l Labels) Set(sender Sender, name string, value float64) {
	l.emit(sender, metrics.MessageGaugeSet{Name: name, Value: value, Labels: l.values})
}

// Count increments a counter, appending any label values the metric declares after these.
func (l Labels) Count(sender Sender, name string, extra ...string) {
	l.emit(sender, metrics.MessageCounterAdd{Name: name, Value: 1, Labels: l.labelValues(extra...)})
}

// Add increments a counter by value, for an event that reports how many of itself it stands for.
func (l Labels) Add(sender Sender, name string, value float64, extra ...string) {
	l.emit(sender, metrics.MessageCounterAdd{Name: name, Value: value, Labels: l.labelValues(extra...)})
}

// Observe records one histogram sample.
func (l Labels) Observe(sender Sender, name string, value float64) {
	l.emit(sender, metrics.MessageHistogramObserve{Name: name, Value: value, Labels: l.values})
}

// emit is best-effort; unconfigured labels stay silent, since a mismatched label count panics radar.
func (l Labels) emit(sender Sender, message any) {
	if sender == nil || len(l.values) == 0 {
		return
	}
	_ = sender.Send(MetricsProcess, message)
}

// labelValues prefixes these label values to a metric's remaining ones.
func (l Labels) labelValues(extra ...string) []string {
	if len(extra) == 0 {
		return l.values
	}
	return append(append(make([]string, 0, len(l.values)+len(extra)), l.values...), extra...)
}

// AvailabilityValue maps availability onto a gauge an operator can alert on: below 2 is degraded.
func AvailabilityValue(availability runtime.Availability) float64 {
	switch availability {
	case runtime.AvailabilityReady:
		return 2
	case runtime.AvailabilityDegraded:
		return 1
	default:
		return 0
	}
}

// Result labels an outcome for a metric's result label.
func Result(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// TerminationReason labels an exit by whether it was asked for.
func TerminationReason(reason error) string {
	switch reason {
	case gen.TerminateReasonNormal:
		return "normal"
	case gen.TerminateReasonShutdown:
		return "shutdown"
	default:
		return "failure"
	}
}

// ElapsedSeconds converts a start timestamp into a sample, false when the work was never timed.
func ElapsedSeconds(start time.Time) (float64, bool) {
	if start.IsZero() {
		return 0, false
	}
	return time.Since(start).Seconds(), true
}
