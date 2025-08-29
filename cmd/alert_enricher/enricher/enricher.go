package enricher

import (
	stdctx "context"
	"log"
	"sync"

	"github.com/harishhary/blink/cmd/alert_enricher/internal/message"
	"github.com/harishhary/blink/internal/configuration"
	ctx "github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/pkg/alerts"
	"github.com/harishhary/blink/pkg/enrichments"
	"golang.org/x/sync/semaphore"
)

// EnricherService enriches alerts with external data and publishes to formatter.
// EnricherService enriches alerts with external data and publishes to formatter.
type EnricherService struct {
	ctx.ServiceContext
	syncMessages     messaging.MessageQueue
	enricherMessages messaging.MessageQueue

	// phases holds the enrichment execution phases (dependencies resolved)
	phases    [][]enrichments.IEnrichment
	phaseLock sync.RWMutex

	// semaphore to bound concurrent enrichments
	sem *semaphore.Weighted
}

// New constructs an alert enricher service.
func New() *EnricherService {
	serviceContext := ctx.New("BLINK-ALERT-ENRICHER - ENRICH")
	if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
		log.Fatalln(err)
	}
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	// initialize semaphore for concurrency (from config or default)
	maxConc := serviceContext.Configuration().Kafka.MaxConcurrentEnrich
	if maxConc <= 0 {
		maxConc = 10
	}
	sem := semaphore.NewWeighted(int64(maxConc))

	return &EnricherService{
		ServiceContext:   serviceContext,
		syncMessages:     serviceContext.Messages().Subscribe(message.SyncService, false),
		enricherMessages: serviceContext.Messages().Subscribe(message.EnricherService, true),
		sem:              sem,
	}
}

// Name returns the enricher service name.
func (service *EnricherService) Name() string { return "alert-enricher" }

// Run applies enrichments on incoming alerts and publishes to the formatter stage.
func (service *EnricherService) Run() errors.Error {
	enricherRepo := enrichments.GetEnrichmentRepository()

	// plugin-sync loop: handle new/unregistered enrichers, rebuild phases
	go func() {
		for range service.syncMessages {
			enricherRepo.Record(<-service.syncMessages)
			all := enricherRepo.All()
			phases, err := buildPhases(all)
			if err != nil {
				service.Error(errors.NewE(err))
				continue
			}
			service.phaseLock.Lock()
			service.phases = phases
			service.phaseLock.Unlock()
			service.Info("rebuilt enrichment phases—%d phases", len(phases))
		}
	}()

	// event loop: apply existing phases to each alert
	for msg := range service.enricherMessages {
		alertMsg, ok := msg.(alerts.AlertMessage)
		if !ok {
			service.Error(errors.New("invalid message type"))
			continue
		}
		alert := alertMsg.Alert
		service.Info("enriching alert %s", alert.AlertID)

		service.phaseLock.RLock()
		phases := service.phases
		service.phaseLock.RUnlock()

		for _, phase := range phases {
			var wg sync.WaitGroup
			for _, enr := range phase {
				if !enr.Enabled() {
					continue
				}
				wg.Add(1)
				go func(enr enrichments.IEnrichment) {
					defer wg.Done()
					if err := service.sem.Acquire(service.Context(), 1); err != nil {
						service.Error(errors.NewE(err))
						return
					}
					defer service.sem.Release(1)

					cctx, cancel := stdctx.WithTimeout(service.Context(), service.Configuration().Kafka.EnrichmentTimeout)
					defer cancel()
					if err := enr.Enrich(cctx, &alert); err != nil {
						service.Error(errors.NewF("enrichment %s failed: %v", enr.Name(), err))
					}
				}(enr)
			}
			wg.Wait()
		}
		service.Messages().Publish(message.FormatService, alerts.AlertMessage{Alert: alert})
	}
}
