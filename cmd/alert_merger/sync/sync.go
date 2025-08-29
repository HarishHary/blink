package sync

import (
   "log"
   "os"

   "github.com/harishhary/blink/cmd/alert_merger/internal/message"
   "github.com/harishhary/blink/internal/configuration"
   "github.com/harishhary/blink/internal/context"
   "github.com/harishhary/blink/internal/logger"
   "github.com/harishhary/blink/internal/messaging"
   "github.com/harishhary/blink/pkg/rules"
)

// SyncService hot-loads alert merger configuration and emits sync messages.
// SyncService hot-loads rule plugins and publishes register/unregister messages.
type SyncService struct {
   ctx         context.ServiceContext
   syncMessages messaging.MessageQueue
}

// New constructs the alert-merger sync service.
// New constructs the alert-merger sync service.
func New() *SyncService {
   ctx := context.New("BLINK-ALERT-MERGER - SYNC")
   if err := configuration.LoadFromEnvironment(&ctx); err != nil {
       log.Fatalln(err)
   }
   ctx.Logger = logger.New(ctx.Name(), "dev")

   repoDir := os.Getenv("RULE_PLUGIN_DIR")
   rules.GetRuleRepository().Load(repoDir)

   return &SyncService{
       ctx:          ctx,
       syncMessages: ctx.Messages().Subscribe(message.SyncService, false),
   }
}

// Name returns the sync service name.
// Name returns the sync service name.
func (s *SyncService) Name() string { return "alert-merger-sync" }

// Run blocks and manages initial sync for alert merging.
// Run starts the hot-loading loop and blocks indefinitely.
func (s *SyncService) Run() errors.Error {
   for msg := range s.syncMessages {
       s.ctx.Debug("sync message: %v", msg)
       rules.GetRuleRepository().Record(msg)
   }
   select {}
}
