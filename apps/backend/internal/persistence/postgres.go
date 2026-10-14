package persistence

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/persistence/db"
)

type Database struct {
	Pool    *pgxpool.Pool
	Queries *db.Queries
}

func NewDatabase(ctx context.Context, cfg *config.Config) (*Database, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database url: %w", err)
	}

	poolConfig.MaxConns = int32(cfg.DBMaxOpenConns)
	poolConfig.MinConns = int32(cfg.DBMaxIdleConns)
	poolConfig.MaxConnLifetime = cfg.DBConnMaxLifetime

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	return &Database{
		Pool:    pool,
		Queries: db.New(pool),
	}, nil
}

func (d *Database) Ping(ctx context.Context) (time.Duration, error) {
	if d == nil || d.Pool == nil {
		return 0, fmt.Errorf("database connection pool is nil")
	}

	start := time.Now()
	err := d.Pool.Ping(ctx)
	latency := time.Since(start)

	if err != nil {
		return latency, fmt.Errorf("database ping failed: %w", err)
	}

	return latency, nil
}

func (d *Database) Close() {
	if d != nil && d.Pool != nil {
		d.Pool.Close()
	}
}
