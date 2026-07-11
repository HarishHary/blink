package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/harishhary/blink/internal/errors"
)

// Logger writes structured JSON logs. New supplies the service context shared by
// all derived loggers.
type Logger struct {
	logger *slog.Logger
}

func New(service string, environment string) *Logger {
	return newLogger(os.Stdout, service, environment)
}

func newLogger(output io.Writer, service string, environment string) *Logger {
	level := slog.LevelInfo
	if environment == "dev" {
		level = slog.LevelDebug
	}
	return &Logger{
		logger: slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})).With("service", service),
	}
}

// With returns an immutable child logger that includes args on every record.
func (log *Logger) With(args ...any) *Logger {
	return &Logger{logger: log.logger.With(args...)}
}

func (log *Logger) Debug(message string, v ...any) {
	log.logger.Debug(fmt.Sprintf(message, v...))
}

func (log *Logger) Info(message string, v ...any) {
	log.logger.Info(fmt.Sprintf(message, v...))
}

func (log *Logger) Error(err errors.Error) {
	log.logger.Error(err.Error())
}

func (log *Logger) ErrorF(message string, v ...any) {
	log.logger.Error(fmt.Sprintf(message, v...))
}
