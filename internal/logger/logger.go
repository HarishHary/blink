package logger

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/harishhary/blink/internal/errors"
)

type Environment = int

var EnvironmentEnum = struct {
	dev         Environment
	staging     Environment
	integration Environment
	prod        Environment
}{
	dev:         0,
	staging:     1,
	integration: 2,
	prod:        3,
}

type ILogger interface {
	Debug(message string, v ...any)
	Info(message string, v ...any)
	Error(err errors.Error)
	ErrorF(message string, v ...any)
}

type Logger struct {
	environment Environment
	logger      *log.Logger
}

func New(prefix string, environment string) *Logger {
	var env = EnvironmentEnum.integration
	switch {
	case environment == "dev":
		env = EnvironmentEnum.dev
	case environment == "integration":
		env = EnvironmentEnum.integration
	case environment == "staging":
		env = EnvironmentEnum.staging
	case environment == "prod":
		env = EnvironmentEnum.prod
	default:
		env = EnvironmentEnum.integration
	}
	return &Logger{
		environment: env,
		logger:      log.New(os.Stdout, fmt.Sprintf("[%s]", prefix), 0),
	}
}

func (log *Logger) Debug(message string, v ...any) {
	if log.environment > 0 {
		return
	}

	msg := fmt.Sprintf(message, v...)
	log.logger.Printf("[%s][\033[1;35mDEBUG\033[0m] %s", time.Now().Format("2006/01/02 - 15:04:05"), msg)
}

func (log *Logger) Info(message string, v ...any) {
	if log.environment > 1 {
		return
	}

	msg := fmt.Sprintf(message, v...)
	log.logger.Printf("[%s][\033[1;36mINFO\033[0m] %s", time.Now().Format("2006/01/02 - 15:04:05"), msg)
}

func (log *Logger) Error(err errors.Error) {
	if log.environment > 1 {
		return
	}

	msg := err.Error()
	log.logger.Printf("[%s][\033[1;31mERROR\033[0m] %s", time.Now().Format("2006/01/02 - 15:04:05"), msg)
}

func (log *Logger) ErrorF(message string, v ...any) {
	if log.environment > 1 {
		return
	}

	msg := fmt.Sprintf(message, v...)
	log.logger.Printf("[%s][\033[1;31mERROR\033[0m] %s", time.Now().Format("2006/01/02 - 15:04:05"), msg)
}
