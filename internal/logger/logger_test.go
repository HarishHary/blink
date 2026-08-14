package logger

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/harishhary/blink/internal/errors"
)

func decodeRecords(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(output)
	var records []map[string]any
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestLoggerUsesJSONAndPreservesChildContext(t *testing.T) {
	var output bytes.Buffer
	root := newLogger(&output, "controller", "dev")
	child := root.With("plugin_type", "rules", "component", "local_reader")

	child.Debug("loaded %d plugins", 2)
	root.Info("ready")

	records := decodeRecords(t, &output)
	if len(records) != 2 {
		t.Fatalf("want 2 records, got %d", len(records))
	}
	if records[0]["level"] != "DEBUG" || records[0]["msg"] != "loaded 2 plugins" || records[0]["service"] != "controller" || records[0]["plugin_type"] != "rules" || records[0]["component"] != "local_reader" {
		t.Fatalf("unexpected child record: %#v", records[0])
	}
	if records[1]["service"] != "controller" || records[1]["plugin_type"] != nil || records[1]["component"] != nil {
		t.Fatalf("root logger was mutated by With: %#v", records[1])
	}
}

func TestLoggerLevelsAndErrors(t *testing.T) {
	for _, environment := range []string{"dev", "integration", "staging", "prod"} {
		t.Run(environment, func(t *testing.T) {
			var output bytes.Buffer
			log := newLogger(&output, "test", environment)
			log.Debug("debug")
			log.Info("info")
			log.Error(errors.NewF("error"))
			log.ErrorF("error %d", 2)

			records := decodeRecords(t, &output)
			want := 3
			if environment == "dev" {
				want = 4
			}
			if len(records) != want {
				t.Fatalf("want %d records, got %d", want, len(records))
			}
			if records[len(records)-1]["level"] != "ERROR" || records[len(records)-1]["msg"] != "error 2" {
				t.Fatalf("ErrorF record was not preserved: %#v", records[len(records)-1])
			}
		})
	}
}

func TestLoggerFatalF(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoggerFatalFHelper$")
	cmd.Env = append(os.Environ(), "LOGGER_FATALF_HELPER=1")
	output, err := cmd.Output()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got %v", err)
	}

	var record map[string]any
	if err := json.Unmarshal(output, &record); err != nil {
		t.Fatal(err)
	}
	if record["level"] != "ERROR" || record["msg"] != "fatal 1" {
		t.Fatalf("unexpected fatal record: %#v", record)
	}
}

func TestLoggerFatalFHelper(t *testing.T) {
	if os.Getenv("LOGGER_FATALF_HELPER") == "1" {
		newLogger(os.Stdout, "test", "dev").FatalF("fatal %d", 1)
	}
}
