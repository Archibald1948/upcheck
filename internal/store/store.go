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

	// 빈 식별자 import ("blank import").
	// 이 패키지의 함수를 직접 부르지는 않지만, init() 이 돌면서
	// database/sql 에 "sqlite" 드라이버를 등록해주는 게 목적이다.
	// _ 를 안 붙이면 "안 쓰는 import" 컴파일 에러가 난다.
	_ "modernc.org/sqlite"
)

// migrationsFS 는 마이그레이션 SQL 을 바이너리 안에 박아 넣는다.
//
// go:embed 지시문은 바로 아래 변수에 파일 내용을 채워 넣으라는 뜻이다.
// 덕분에 배포할 때 바이너리 하나만 던지면 된다 — SQL 파일을 따로 안 들고 다녀도 된다.
// 주의: //go:embed 앞에 공백이 있으면 안 되고, 바로 다음 줄이 변수 선언이어야 한다.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store 는 DB 핸들과 그 위의 질의들을 들고 있다.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// Open 은 SQLite 파일을 열고 마이그레이션을 최신 상태로 올린다.
//
// path 가 ":memory:" 면 메모리 DB 를 쓴다 (테스트용).
func Open(ctx context.Context, path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}

	// 연결 문자열에 PRAGMA 를 붙인다. modernc.org/sqlite 는
	// _pragma=... 쿼리 파라미터를 연결할 때마다 실행해준다.
	//
	// 연결마다 걸어야 한다는 게 중요하다. database/sql 은 커넥션 풀이라
	// 한 번 실행한 PRAGMA 가 다른 커넥션에는 적용되지 않는다.
	dsn := path
	if path != ":memory:" {
		q := url.Values{}
		// WAL: 읽기와 쓰기가 서로를 막지 않는다. 기본 journal 모드에서는
		// 쓰기가 읽기를 막아서 집계 질의 중에 저장이 멈춘다. (스펙 7절 함정)
		q.Add("_pragma", "journal_mode(WAL)")
		// busy_timeout: 잠겨 있으면 즉시 실패하지 말고 이만큼 기다린다.
		// 이게 없으면 "database is locked" 에러를 직접 재시도해야 한다.
		q.Add("_pragma", "busy_timeout(5000)")
		// foreign_keys: SQLite 는 외래키 검사가 기본 꺼져 있다. 켜야 한다.
		q.Add("_pragma", "foreign_keys(1)")
		// synchronous=NORMAL: WAL 에서는 이게 권장값이다. FULL 보다 훨씬 빠르고,
		// 전원이 나가도 DB 가 깨지지 않는다 (마지막 몇 트랜잭션은 날아갈 수 있다).
		q.Add("_pragma", "synchronous(NORMAL)")
		dsn = path + "?" + q.Encode()
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("DB 열기 실패: %w", err)
	}

	// SQLite 는 쓰기를 하나만 허용한다. 커넥션을 여러 개 두면
	// 그중 하나만 쓸 수 있고 나머지는 잠금을 기다린다.
	// 쓰기를 한 goroutine 으로 직렬화하는 우리 구조와 맞춰 1로 둔다.
	// (읽기까지 직렬화되지만, 이 규모에서는 문제가 되지 않는다)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // 파일 DB라 커넥션을 재활용해도 문제없다

	// sql.Open 은 실제로 연결하지 않는다. 여기서 처음 확인한다.
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
//
// 마이그레이션을 쓰는 이유: 스키마를 코드로 관리하면 "이 DB가 지금 몇 번째
// 버전인가"를 goose_db_version 테이블이 기억한다. 이미 적용된 건 건너뛰므로
// 프로그램을 몇 번을 재시작해도 안전하다.
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
//
// Go에는 try/finally 가 없어서 "성공하면 커밋, 아니면 롤백"을 매번
// 손으로 쓰면 빠뜨리기 쉽다. 이렇게 감싸두면 호출부는 fn 만 쓰면 된다.
// (고차 함수 — 함수를 인자로 받는 함수)
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("트랜잭션 시작 실패: %w", err)
	}
	// 이미 커밋된 트랜잭션에 Rollback 을 불러도 sql.ErrTxDone 이 날 뿐 해가 없다.
	// 그래서 무조건 defer 로 걸어두면 어느 경로로 빠져나가도 정리된다.
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
