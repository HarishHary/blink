package services

import (
	"log"
	"os"
	"sync"
	"time"
)

type Runner struct {
	inits    []Service
	services []Service

	logger *log.Logger
}

func New() *Runner {
	return &Runner{
		services: make([]Service, 0),
		logger:   log.New(os.Stdout, "[SERVICE - RUNNER] ", log.Ldate|log.Ltime),
	}
}

func (runner *Runner) RegisterInit(services ...Service) {
	runner.inits = append(runner.inits, services...)
}

func (runner *Runner) Register(services ...Service) {
	runner.services = append(runner.services, services...)
}

func (runner *Runner) Run() {
	initGroup := sync.WaitGroup{}
	for i := range runner.inits {
		initGroup.Add(1)

		init := runner.inits[i]
		go func() {
			runner.logger.Printf("service %s started\n", init.Name())
			if err := init.Run(); err != nil {
				runner.logger.Printf("init service %s terminated with an error\n%s\n", init.Name(), err)
			} else {
				runner.logger.Printf("service %s terminated successfully\n", init.Name())
			}

			initGroup.Done()
		}()
	}

	initGroup.Wait()

	for _, service := range runner.services {
		go runner.run(service)
	}

	// pause indefinitely
	select {}
}

func (runner *Runner) run(service Service) {
	for {
		runner.logger.Printf("service %s started\n", service.Name())
		if err := service.Run(); err != nil {
			runner.logger.Printf("service %s terminated with an error\n%s\n", service.Name(), err)
		}
		runner.logger.Printf("service %s terminated, restarting in 5 seconds\n", service.Name())

		time.Sleep(5 * time.Second)
	}
}
