package rules_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/harishhary/blink/internal/logger"
	"github.com/harishhary/blink/internal/messaging"
	"github.com/harishhary/blink/internal/plugin"
	"github.com/harishhary/blink/internal/services"
	"github.com/harishhary/blink/pkg/rules"
)

const (
	testPluginPkg     = "github.com/harishhary/blink/pkg/rules/testdata/simple_rule"
	testPluginBinName = "simple_rule"
	registerTimeout   = 15 * time.Second
)

// testSidecarYAML is the YAML sidecar for the simple_rule test plugin binary.
// The name field must match the binary base name ("simple_rule").
const testSidecarYAML = `
id: "test-simple-rule-id"
name: "simple_rule"
display_name: "simple-rule"
description: "always matches - used for integration tests"
enabled: true
version: "1.0.0"
severity: "info"
confidence: "low"
log_types: ["test"]
`

// buildPlugin compiles the test plugin binary into dir and returns its path.
func buildPlugin(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, testPluginBinName)
	cmd := exec.Command("go", "build", "-o", out, testPluginPkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build plugin: %v", err)
	}
	return out
}

// writeSidecar writes the test YAML sidecar to dir.
func writeSidecar(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, testPluginBinName+".yaml")
	if err := os.WriteFile(path, []byte(testSidecarYAML), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// waitForRegister blocks until the channel receives a RegisterMessage for the
// given rule name, or the timeout elapses.
func waitForRegister(t *testing.T, ch <-chan messaging.Message, name string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-ch:
			if rm, ok := msg.(plugin.RegisterMessage[rules.Rule]); ok {
				if len(rm.Items) > 0 && rm.Items[0].RuleMetadata().Name == name {
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}

func waitForUnregister(t *testing.T, ch <-chan messaging.Message, name string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-ch:
			if um, ok := msg.(plugin.UnregisterMessage[rules.Rule]); ok {
				if um.ItemKey.Id == name {
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}

func waitForRemove(t *testing.T, ch <-chan messaging.Message, id string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-ch:
			if rm, ok := msg.(plugin.RemoveMessage[rules.Rule]); ok {
				if rm.ItemKey.Id == id {
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}

func TestManagerHotReload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()

	// Write the YAML sidecar before starting the manager so the watcher has it
	// available when the binary appears.
	writeSidecar(t, dir)

	cfgMgr := rules.NewRuleConfigManager(logger.New("test-config", "dev"), dir)
	cfgSvc := services.NewConfigSyncService("test-config", "test-config", cfgMgr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cfgSvc.Run(ctx) //nolint:errcheck

	// Use a buffered channel as the notify sink - replaces the old message bus.
	events := make(chan messaging.Message, 64)
	notify := func(msg messaging.Message) { events <- msg }

	log := logger.New("rules-manager-test", "dev")
	mgr := rules.NewRulePluginManager(log, notify, dir, cfgMgr)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Build and drop the plugin binary - manager should pick it up.
	binPath := buildPlugin(t, dir)

	if !waitForRegister(t, events, "simple_rule", registerTimeout) {
		t.Fatal("timed out waiting for RegisterMessage after binary appears")
	}

	// Remove the binary - expect a RemoveMessage (permanent deletion, not transient stop).
	if err := os.Remove(binPath); err != nil {
		t.Fatalf("remove binary: %v", err)
	}

	// ItemID is the stable plugin ID (from YAML id: field), not the display name.
	if !waitForRemove(t, events, "test-simple-rule-id", registerTimeout) {
		t.Fatal("timed out waiting for RemoveMessage after binary removed")
	}
}
