package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"

	"followapi/internal/config"
)

type DB struct {
	*sql.DB
	Driver string // "mysql" or "sqlite"
}

type Queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func Open(cfg config.Config) (*DB, error) {
	if cfg.DBConnection == "sqlite" {
		dsn := cfg.DBDatabase
		if dsn == "" {
			dsn = ":memory:"
		}
		if !strings.Contains(dsn, "?") {
			dsn += "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
		}
		sqldb, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		sqldb.SetMaxOpenConns(1)
		if err := sqldb.Ping(); err != nil {
			_ = sqldb.Close()
			return nil, err
		}
		return &DB{DB: sqldb, Driver: "sqlite"}, nil
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=false&interpolateParams=true",
		cfg.DBUsername, cfg.DBPassword, cfg.DBHost, cfg.DBPort, cfg.DBDatabase,
	)
	sqldb, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	sqldb.SetMaxOpenConns(25)
	sqldb.SetMaxIdleConns(10)
	sqldb.SetConnMaxLifetime(5 * time.Minute)
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return &DB{DB: sqldb, Driver: "mysql"}, nil
}

func (d *DB) IsSQLite() bool {
	return d.Driver == "sqlite"
}

func (d *DB) ForUpdate() string {
	if d.Driver == "mysql" {
		return " FOR UPDATE"
	}
	return ""
}

func (d *DB) InsertIgnore() string {
	if d.Driver == "sqlite" {
		return "INSERT OR IGNORE"
	}
	return "INSERT IGNORE"
}

func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ParseTime flexibly parses time values scanned from MySQL or SQLite TEXT timestamps.
func ParseTime(v any) *time.Time {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case time.Time:
		u := t.UTC()
		return &u
	case []byte:
		return ParseTimeString(string(t))
	case string:
		return ParseTimeString(t)
	default:
		return ParseTimeString(fmt.Sprint(v))
	}
}

func ParseTimeString(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" || s == "<nil>" {
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			u := parsed.UTC()
			return &u
		}
	}
	return nil
}

func ParseDateOnly(v any) *string {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case time.Time:
		s := t.Format("2006-01-02")
		return &s
	case []byte:
		return parseDateStr(string(t))
	case string:
		return parseDateStr(t)
	default:
		return parseDateStr(fmt.Sprint(v))
	}
}

func parseDateStr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" || s == "<nil>" {
		return nil
	}
	if len(s) >= 10 {
		d := s[:10]
		return &d
	}
	return &s
}

func FormatDBTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "error 1062")
}

// Placeholders returns "?, ?, ..." of length n.
func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func Int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
