// Package store 는 체크 결과를 SQLite 에 저장하고 집계한다.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	"github.com/pressly/goose/v3"

	_ "modernc.org/sqlite" // "sqlite" 드라이버 등록
)

// migrationsFS 는 마이그레이션 SQL 을 바이너리 안에 박아 넣는다.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store 는 DB 핸들과 그 위의 질의들을 들고 있다.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// Open 은 SQLite 파일을 열고 마이그레이션을 최신 상태로 올린다.
// path 가 ":memory:" 면 메모리 DB 를 쓴다 (테스트용).
func Open(ctx context.Context, path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}

	// PRAGMA 는 커넥션마다 걸어야 해서 DSN 에 붙인다.
	dsn := path
	if path != ":memory:" {
		q := url.Values{}
		q.Add("_pragma", "journal_mode(WAL)") // 쓰기가 읽기를 막지 않게
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "foreign_keys(1)") // SQLite 는 기본 꺼져 있다
		q.Add("_pragma", "synchronous(NORMAL)")
		dsn = path + "?" + q.Encode()
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("DB 열기 실패: %w", err)
	}

	// SQLite 는 쓰기를 하나만 허용하므로 커넥션도 1개로 둔다.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("DB 연결 실패: %w", err)
	}

	s := &Store{db: db, log: log}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate 는 goose 로 스키마를 최신 상태까지 올린다.
func (s *Store) migrate() error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger()) // goose 가 stdout 에 직접 찍는 걸 막는다

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("goose 방언 설정 실패: %w", err)
	}
	if err := goose.Up(s.db, "migrations"); err != nil {
		return fmt.Errorf("마이그레이션 실패: %w", err)
	}

	v, err := goose.GetDBVersion(s.db)
	if err == nil {
		s.log.Debug("스키마 준비 완료", "version", v)
	}
	return nil
}

// Ping 은 DB 가 응답하는지 확인한다. 준비 상태(readiness) 점검에 쓴다.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close 는 DB 를 닫는다.
func (s *Store) Close() error { return s.db.Close() }

// DB 는 테스트에서 직접 질의할 때 쓴다.
func (s *Store) DB() *sql.DB { return s.db }

// tx 는 함수를 트랜잭션 안에서 실행한다.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("트랜잭션 시작 실패: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("커밋 실패: %w", err)
	}
	return nil
}

// ─────────────────────── 시간 다루기 ───────────────────────

// unix 는 시각을 저장 형식(UTC epoch 초)으로 바꾼다.
func unix(t time.Time) int64 { return t.UTC().Unix() }

// fromUnix 는 저장된 값을 time.Time 으로 되돌린다. 항상 UTC 다.
func fromUnix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

// truncHour 는 시각을 정시로 내린다. 롤업 버킷 경계를 구할 때 쓴다.
func truncHour(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }

// discard 는 로그를 버리는 로거다. 테스트에서 쓴다.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
