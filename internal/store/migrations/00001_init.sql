-- +goose Up
-- +goose StatementBegin

-- 시각은 전부 UNIX epoch 초(UTC)로 저장한다.
--
-- SQLite 에는 날짜/시간 타입이 아예 없다. TEXT(ISO8601), INTEGER(epoch),
-- REAL(율리우스일) 중 하나를 골라야 한다. 여기서는 INTEGER 를 쓴다.
--   - 8바이트로 작고 비교가 빠르다
--   - 시간 버킷 계산이 나눗셈 한 번이다: checked_at / 3600 * 3600
--     (TEXT 였다면 문자열 함수로 잘라 붙여야 한다 — 롤업에서 매번 쓴다)
-- 사람이 볼 때는 datetime(checked_at, 'unixepoch') 로 변환한다.

CREATE TABLE monitors (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL UNIQUE,
    type            TEXT    NOT NULL,
    target          TEXT    NOT NULL,
    interval_sec    INTEGER NOT NULL,
    timeout_ms      INTEGER NOT NULL,
    expected_status INTEGER NOT NULL,
    keyword         TEXT    NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL
);

CREATE TABLE checks (
    id          INTEGER PRIMARY KEY,
    monitor_id  INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    checked_at  INTEGER NOT NULL,
    ok          INTEGER NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    latency_ms  INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT ''
);

-- 거의 모든 조회가 "특정 모니터의 특정 기간"이라 복합 인덱스를 건다.
-- 컬럼 순서가 중요하다: monitor_id 로 먼저 좁히고 checked_at 으로 범위를 자른다.
-- 반대 순서였다면 monitor_id 조건을 인덱스로 못 쓴다.
CREATE INDEX idx_checks_monitor_time ON checks(monitor_id, checked_at);

-- 정리 잡이 "오래된 행 전부"를 지울 때 쓰는 인덱스.
CREATE INDEX idx_checks_time ON checks(checked_at);

-- 시간별 롤업.
--
-- checks 는 금방 커진다. 모니터 100개 × 60초 주기 = 하루 14만 행.
-- 한 시간 치를 한 행으로 접어 두면 90일 보관해도 모니터당 2160행이다.
--
-- total/ok_count 는 더하면 그대로 정확한 값이 나온다(결합법칙이 성립).
-- 반면 p50/p95 는 합칠 수 없다 — 백분위수의 백분위수는 원래 값이 아니다.
-- 그래서 긴 구간의 응답시간은 근사치이고, 코드에서 그렇게 표시한다.
CREATE TABLE checks_hourly (
    monitor_id  INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    hour        INTEGER NOT NULL,   -- 정시 epoch (checked_at / 3600 * 3600)
    total       INTEGER NOT NULL,
    ok_count    INTEGER NOT NULL,
    latency_p50 INTEGER NOT NULL,
    latency_p95 INTEGER NOT NULL,
    latency_max INTEGER NOT NULL,
    PRIMARY KEY (monitor_id, hour)
) WITHOUT ROWID;
-- WITHOUT ROWID: PK 자체가 저장 순서가 되어 중복 인덱스가 없어진다.
-- (monitor_id, hour) 로만 접근하는 테이블이라 딱 맞는다.

-- 장애 구간. M2에서는 테이블만 만들고, M3(알림)에서 상태 전이를 판정해 채운다.
CREATE TABLE incidents (
    id          INTEGER PRIMARY KEY,
    monitor_id  INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    started_at  INTEGER NOT NULL,
    resolved_at INTEGER,            -- NULL = 아직 진행 중
    cause       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_incidents_monitor ON incidents(monitor_id, started_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE incidents;
DROP TABLE checks_hourly;
DROP TABLE checks;
DROP TABLE monitors;
-- +goose StatementEnd
