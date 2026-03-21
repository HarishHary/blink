package backends

import (
	"context"
	"database/sql"
	"fmt"

	backends "github.com/harishhary/blink/internal/backends/sqllite"
	_ "github.com/snowflakedb/gosnowflake"
)

type SnowflakeBackend struct {
	backends.SQLiteBackend
}

func NewSnowflakeBackend(ctx context.Context, dbName string) (*SnowflakeBackend, error) {
	db, err := sql.Open("snowflake", dbName)
	if err != nil {
		return nil, fmt.Errorf("failed to open Snowflake database: %w", err)
	}

	return &SnowflakeBackend{
		SQLiteBackend: backends.SQLiteBackend{
			Ctx:    ctx,
			Db:     db,
			DbName: dbName,
		},
	}, nil
}
