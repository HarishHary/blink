package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/harishhary/blink/internal/executor"
	"github.com/harishhary/blink/internal/pools"
	"go.yaml.in/yaml/v4"
)

// ValidationError describes a single configuration problem found during directory validation.
type ValidationError struct {
	File     string
	Field    string
	PluginID string
	Blocking bool
	Message  string
}

func (e ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s [%s]: %s", e.File, e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.File, e.Message)
}

// Loader[T] encapsulates per-plugin-type config loading logic.
// Implement once per plugin type and inject into NewConfigManager.
// Embed BaseLoader[T] to inherit default Validate and CrossValidate.
type Loader[T executor.Syncable] interface {
	// Parse reads a single YAML sidecar file and returns the parsed metadata.
	Parse(path string) (T, error)
	// Validate runs directory-level checks given already-parsed items and
	// executable binary names present in the directory.
	Validate(items []T, binaries []string) []ValidationError
	// CrossValidate runs cross-item checks (e.g. dependency cycle detection).
	CrossValidate(all []T) error
}

// BaseLoader[U, T] provides default implementations of Parse, Validate, and
// CrossValidate. Embed in per-plugin Loader structs to inherit defaults and only
// override what the plugin type needs. U is the concrete struct (e.g. FormatterMetadata);
// T is its pointer type (e.g. *FormatterMetadata).
type BaseLoader[U executor.Syncable, T interface {
	*U
	executor.Syncable
}] struct{}

// named is satisfied by any metadata type that embeds *executor.PluginMetadata.
// It is intentionally unexported — name injection is a loader concern, not a plugin runtime concern.
type named interface{ SetName(string) }

func (BaseLoader[U, T]) Parse(path string) (T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg U
	p := T(&cfg)
	if err := yaml.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if n, ok := any(p).(named); ok {
		n.SetName(stem)
	}
	return p, nil
}

func (BaseLoader[U, T]) Validate(items []T, binaries []string) []ValidationError {
	var errs []ValidationError

	binarySet := make(map[string]struct{}, len(binaries))
	for _, b := range binaries {
		binarySet[b] = struct{}{}
	}

	byID := make(map[string][]T)
	nameSet := make(map[string]struct{}, len(items))

	for _, item := range items {
		m := item.Metadata()
		if m.Id == "" {
			errs = append(errs, ValidationError{File: m.Name + ".yaml", Field: "id", Blocking: true, Message: "required field missing"})
		}
		nameSet[m.Name] = struct{}{}
		if m.Enabled {
			if _, ok := binarySet[m.Name]; !ok {
				errs = append(errs, ValidationError{
					File:     m.Name + ".yaml",
					PluginID: m.Id,
					Blocking: true,
					Message:  fmt.Sprintf("enabled but no matching binary found (expected executable %q)", m.Name),
				})
			}
		}
		byID[m.Id] = append(byID[m.Id], item)
	}

	for _, b := range binaries {
		if _, ok := nameSet[b]; !ok {
			errs = append(errs, ValidationError{
				File:    b,
				Message: fmt.Sprintf("binary has no YAML sidecar (expected %q)", b+".yaml"),
			})
		}
	}

	for id, group := range byID {
		if len(group) <= 1 {
			continue
		}
		files := make([]string, len(group))
		stableCount, shadowCount := 0, 0
		for i, item := range group {
			m := item.Metadata()
			files[i] = m.Name + ".yaml"
			if m.RolloutMode == pools.RolloutModeCanary || m.RolloutMode == pools.RolloutModeShadow {
				shadowCount++
			} else {
				stableCount++
			}
		}
		switch {
		case stableCount == 0:
			errs = append(errs, ValidationError{
				Field: "mode", PluginID: id, Blocking: true,
				Message: fmt.Sprintf(
					"plugin id %q has %d versions (%s) but none is stable — all declare shadow/canary; one must omit mode or set mode: \"blue-green\"",
					id, len(group), strings.Join(files, ", "),
				),
			})
		case stableCount > 1:
			errs = append(errs, ValidationError{
				Field: "mode", PluginID: id, Blocking: true,
				Message: fmt.Sprintf(
					"plugin id %q has %d stable versions (%s) — only one may omit mode or use \"blue-green\"; mark the others as \"shadow\" or \"canary\"",
					id, stableCount, strings.Join(files, ", "),
				),
			})
		case shadowCount > 1:
			errs = append(errs, ValidationError{
				Field: "mode", PluginID: id, Blocking: true,
				Message: fmt.Sprintf(
					"plugin id %q has %d shadow/canary versions (%s) — only one non-stable version is supported at a time",
					id, shadowCount, strings.Join(files, ", "),
				),
			})
		}
	}

	return errs
}

func (BaseLoader[U, T]) CrossValidate([]T) error { return nil }
