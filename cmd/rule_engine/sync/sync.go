package sync

import (
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/harishhary/blink/cmd/rule_engine/internal/message"
	"github.com/harishhary/blink/internal/context"
	"github.com/harishhary/blink/internal/errors"
	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/repository"
	"github.com/harishhary/blink/internal/sources/azure_storage"
	"github.com/harishhary/blink/pkg/enrichments"
	"github.com/harishhary/blink/pkg/formatters"
	"github.com/harishhary/blink/pkg/matchers"
	"github.com/harishhary/blink/pkg/rules"
	"github.com/harishhary/blink/pkg/rules/tuning_rules"
)

// Periodically sync the loaded rules with the database
type SyncService struct {
	context.ServiceContext
	syncMessages messaging.MessageQueue
	storage      *azure_storage.Client
}

func New() *SyncService {
	serviceContext := context.New("BLINK-NODE - SYNC")
	// if err := configuration.LoadFromEnvironment(&serviceContext); err != nil {
	// 	log.Fatalln(err)
	// }
	serviceContext.Logger = logger.New(serviceContext.Name(), "dev")

	storageContext := azure_storage.Configuration{}
	// if err := configuration.LoadFromEnvironment(&storageContext); err != nil {
	// 	log.Fatalln(err)
	// }
	storage := azure_storage.New(storageContext, "rules")
	return &SyncService{
		ServiceContext: serviceContext,
		syncMessages:   serviceContext.Messages().Subscribe(message.SyncService, false),
		storage:        storage,
	}
}

func (service *SyncService) FetchRulesFromAzure() (*rules.RuleRepository, errors.Error) {
	// Set the path for temporary directory to store rules
	tempDir, err := os.MkdirTemp("", "rules")
	if err != nil {
		return nil, errors.NewF("Failed to create temporary directory: %s", err)
	}
	defer os.RemoveAll(tempDir) // Clean up the temp directory after loading plugins

	// Set the path where the rules will be downloaded to
	rulesPath := "rules_directory" // Make sure this directory exists or create it
	entries, err := service.storage.List(rulesPath)
	if err != nil {
		return nil, errors.NewF("Failed to list rules in Azure Blob Storage: %s", err)
	}

	var ruleRepo = rules.NewRuleRepository()
	for _, entry := range entries {
		// Download the rule plugin
		var err error
		pluginBlob, err := service.storage.Download(entry.Name)
		if err != nil {
			return nil, errors.NewF("Failed to download rule from Azure Blob Storage: %s", err)
		}

		// Write the pluginBlob to a temporary file
		tempFile, err := os.CreateTemp(tempDir, "*.so")
		if err != nil {
			return nil, errors.NewF("Failed to create temporary file: %s", err)
		}
		defer tempFile.Close()

		if _, err := tempFile.Write(pluginBlob); err != nil {
			return nil, errors.NewF("Failed to write plugin to temporary file: %s", err)
		}
		// Ensure the file is flushed to disk
		tempFile.Sync()

		// Create a temporary file to store the downloaded plugin
		pluginPath := filepath.Join(rulesPath, entry.Name)
		if err := os.WriteFile(pluginPath, pluginBlob, 0644); err != nil {
			return nil, errors.NewF("Failed to write plugin to file: %s", err)
		}

		// Load the rule from the plugin
		if err := ruleRepo.Load(pluginPath); err != nil {
			return nil, errors.NewF("Failed to load rule from plugin: %s", err)
		}
	}
	return ruleRepo, nil
}

func SyncRepositories[T repository.ISyncable](service *SyncService, directory string, repo repository.IRepository[T]) errors.Error {
	tempRepo := repository.NewRepository[T]()
	if err := tempRepo.Load(directory); err != nil {
		return errors.NewE(err)
	}
	service.Debug("running diff for '%s'", reflect.TypeOf(tempRepo).String())
	toAdd, toDelete := repo.Diff(tempRepo)

	if len(toAdd) == 0 && len(toDelete) == 0 {
		service.Debug("no diff detected for '%s'", reflect.TypeOf(tempRepo).String())
		return nil
	}

	service.Info("%d %s to add", len(toAdd), reflect.TypeOf(toAdd).String())
	service.Info("%d %s to delete", len(toDelete), reflect.TypeOf(toDelete).String())

	for _, entry := range toAdd {
		service.Debug("publishing register message for '%s'\n", entry.Name())
		service.Messages().Publish(message.SyncService, repository.NewRegisterMessage[T](entry))
	}
	for _, instanceID := range toDelete {
		service.Debug("publishing unregister message for '%s'\n", instanceID)
		service.Messages().Publish(message.SyncService, repository.NewUnregisterMessage[T](instanceID))
	}
	return nil
}

func (service *SyncService) Run() errors.Error {
	// Define the local directory path to load rules from
	localDirectories := map[string]string{
		"rules":        "/Users/harish.segar/Documents/Research/blink/examples/rules/",
		"enrichments":  "/Users/harish.segar/Documents/Research/blink/examples/enrichments/",
		"tuning_rules": "/Users/harish.segar/Documents/Research/blink/examples/matchers/",
		"matchers":     "/Users/harish.segar/Documents/Research/blink/examples/formatters/",
		"formatters":   "/Users/harish.segar/Documents/Research/blink/examples/tuning-rules/",
	}

	service.Info("getting repositories...")
	ruleRepository := rules.GetRuleRepository()
	enrichmentRepository := enrichments.GetEnrichmentRepository()
	formatterRepository := formatters.GetFormatterRepository()
	matcherRepository := matchers.GetMatcherRepository()
	tuningRuleRepository := tuning_rules.GetTuningRuleRepository()

	service.Info("loading repositories...")
	ruleRepository.Load(localDirectories["rules"])
	enrichmentRepository.Load(localDirectories["enrichments"])
	formatterRepository.Load(localDirectories["formatters"])
	matcherRepository.Load(localDirectories["matchers"])
	tuningRuleRepository.Load(localDirectories["tuning_rules"])

	go func() {
		recv := func() messaging.Message {
			msg := <-service.syncMessages
			service.Debug("received message: '%v'", msg)
			return msg
		}

		for {
			newMessage := recv()
			service.Debug("recording new message: '%v'", newMessage)
			ruleRepository.Record(newMessage)
			enrichmentRepository.Record(newMessage)
			formatterRepository.Record(newMessage)
			matcherRepository.Record(newMessage)
			tuningRuleRepository.Record(newMessage)
		}
	}()

	for {
		service.Info("syncing repositories...")
		time.Sleep(10 * time.Second)

		if err := SyncRepositories[rules.IRule](service, localDirectories["rules"], ruleRepository); err != nil {
			service.Error(err)
		}

		if err := SyncRepositories[enrichments.IEnrichment](service, localDirectories["enrichments"], enrichmentRepository); err != nil {
			service.Error(err)
		}

		if err := SyncRepositories[formatters.IFormatter](service, localDirectories["formatters"], formatterRepository); err != nil {
			service.Error(err)
		}

		if err := SyncRepositories[matchers.IMatcher](service, localDirectories["matchers"], matcherRepository); err != nil {
			service.Error(err)
		}

		if err := SyncRepositories[tuning_rules.ITuningRule](service, localDirectories["tuning_rules"], tuningRuleRepository); err != nil {
			service.Error(err)
		}
	}
}
