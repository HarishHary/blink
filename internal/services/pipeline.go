package services

import (
	"context"

	"github.com/harishhary/blink/internal/broker"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/pkg/alerts"
)

// MaxPluginAttempts is the number of DLQ round-trips an alert makes when a referenced
// plugin is missing before the stage passes the alert through without that plugin.
// This prevents infinite DLQ loops while still retrying transient gaps.
const MaxPluginAttempts = 3

type PipelineCounters struct {
	In         func() // called after successful unmarshal
	Out        func() // called after successful write
	ParseError func() // called when alerts.Unmarshal fails
	WriteError func() // called when Marshal or WriteMessages fails
	DLQ        func() // called when an alert is dead-lettered
}

// RunAlertPipeline is the shared Kafka read => process => write => commit loop for alert pipeline stages (tuner, enricher, formatter).
//
// process mutates alert in-place and returns:
//   - skip=true to suppress the downstream write (e.g. tuning rule marked alert ignored)
//   - deadLetter=true to route the alert to the DLQ writer instead of the forward topic
func RunAlertPipeline(
	ctx context.Context,
	log *logger.Logger,
	reader broker.Reader,
	writer broker.Writer,
	dlq broker.Writer,
	batchSize int,
	counters PipelineCounters,
	process func(ctx context.Context, key []byte, alert *alerts.Alert) (skip bool, deadLetter bool),
) errors.Error {
	incr := func(f func()) {
		if f != nil {
			f()
		}
	}

	for {
		msgs, err := reader.ReadBatch(ctx, batchSize)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error(errors.NewE(err))
			continue
		}

		for _, m := range msgs {
			alert, err := alerts.Unmarshal(m.Value)
			if err != nil {
				incr(counters.ParseError)
				log.Error(errors.NewE(err))
				continue
			}
			incr(counters.In)

			skip, deadLetter := process(ctx, m.Key, alert)

			if deadLetter && dlq != nil {
				payload, merr := alerts.Marshal(alert)
				if merr != nil {
					log.Error(errors.NewE(merr))
					continue
				}
				if werr := dlq.WriteMessages(ctx, broker.Message{Key: m.Key, Value: payload}); werr != nil {
					log.Error(errors.NewE(werr))
				} else {
					incr(counters.DLQ)
				}
				continue
			}

			if skip {
				continue
			}

			// Reset per-stage retry counter before forwarding so the next stage
			// starts with a clean slate independent of retries in this stage.
			alert.Attempts = 0
			payload, merr := alerts.Marshal(alert)
			if merr != nil {
				incr(counters.WriteError)
				log.Error(errors.NewE(merr))
				continue
			}
			if werr := writer.WriteMessages(ctx, broker.Message{Key: m.Key, Value: payload}); werr != nil {
				incr(counters.WriteError)
				log.Error(errors.NewE(werr))
				continue
			}
			incr(counters.Out)
		}

		if err := reader.CommitMessages(ctx, msgs...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error(errors.NewE(err))
		}
	}
}
