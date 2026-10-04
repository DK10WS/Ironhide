package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/dk10ws/ironhide/internal/config"
)

type Database struct {
	db *gorm.DB
}

func Open(ctx context.Context, c config.Postgres) (*Database, error) {
	const (
		attempts = 5
		delay    = 2 * time.Second
	)

	var db *gorm.DB
	var err error

	for i := range attempts {
		db, err = gorm.Open(postgres.Open(c.DSN()), &gorm.Config{
			Logger:  newSlogLogger(),
			NowFunc: func() time.Time { return time.Now().UTC() },
		})
		if err == nil {
			break
		}
		slog.Warn("postgres connect failed, retrying", "attempt", i+1, "err", sanitize(err))
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", sanitize(err))
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("access sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	slog.Info("postgres connected",
		"host", c.Host, "port", c.Port, "database", c.Database,
	)
	return &Database{db: db}, nil
}

func (s *Database) DB() *gorm.DB { return s.db }

func (s *Database) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (s *Database) Ping(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

func (s *Database) Migrate() error {
	if err := s.db.AutoMigrate(&Site{}, &Deployment{}); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	slog.Info("schema migrated", "tables", "sites,deployments")
	return nil
}

func sanitize(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, marker := range []string{"password=", "PGPASSWORD="} {
		if i := strings.Index(msg, marker); i >= 0 {
			end := i + len(marker)
			for end < len(msg) && msg[end] != ' ' {
				end++
			}
			msg = msg[:i+len(marker)] + "***" + msg[end:]
		}
	}
	return errors.New(msg)
}

func newSlogLogger() gormlogger.Interface {
	return gormlogger.New(
		slogWriter{},
		gormlogger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  gormlogger.Warn,
			IgnoreRecordNotFoundError: true,
			ParameterizedQueries:      true,
		},
	)
}

type slogWriter struct{}

func (slogWriter) Printf(format string, args ...any) {
	slog.Debug(fmt.Sprintf(format, args...))
}
