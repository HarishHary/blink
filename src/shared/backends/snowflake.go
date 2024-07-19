package backends

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/snowflakedb/gosnowflake"
)

type SnowflakeBackend struct {
	SQLiteBackend
}

func NewSnowflakeBackend(ctx context.Context, dbName string) (*SnowflakeBackend, error) {
	db, err := sql.Open("snowflake", dbName)
	if err != nil {
		return nil, fmt.Errorf("failed to open Snowflake database: %w", err)
	}

	return &SnowflakeBackend{
		SQLiteBackend: SQLiteBackend{
			ctx:    ctx,
			db:     db,
			dbName: dbName,
		},
	}, nil
}
