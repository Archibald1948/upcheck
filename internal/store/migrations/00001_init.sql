-- +goose Up
-- +goose StatementBegin

-- 시각은 전부 UNIX epoch 초(UTC)로 저장한다.

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

CREATE INDEX idx_checks_monitor_time ON checks(monitor_id, checked_at);

-- 정리 잡이 "오래된 행 전부"를 지울 때 쓰는 인덱스.
CREATE INDEX idx_checks_time ON checks(checked_at);

-- 시간별 롤업. p50/p95 는 합칠 수 없어서 긴 구간의 응답시간은 근사치다.
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

-- 장애 구간.
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
