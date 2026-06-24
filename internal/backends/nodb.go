package backends

import (
	"context"

	"github.com/harishhary/blink/internal/controller/model"
)

// nopDatabase is a no-op Database for offline development, single-node
// deployments, and tests — nothing is persisted and generation always reloads as 0.
type nopDatabase struct{}

// NewNop returns a no-op Database.
func NewNop() Database { return nopDatabase{} }

func (nopDatabase) LoadAll(context.Context) ([]model.ControllerRecord, error) { return nil, nil }
func (nopDatabase) Upsert(context.Context, []model.ControllerRecord) error    { return nil }
func (nopDatabase) LoadGeneration(context.Context) (int64, error)             { return 0, nil }
func (nopDatabase) SaveGeneration(context.Context, int64) error               { return nil }
func (nopDatabase) SaveSnapshot(context.Context, model.Snapshot) error        { return nil }

var _ Database = nopDatabase{}
