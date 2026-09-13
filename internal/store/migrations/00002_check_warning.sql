-- +goose Up
-- +goose StatementBegin

-- warning 은 "실패는 아니지만 알아둘 것"이다. 지금은 tls 인증서 만료 임박에 쓴다.
--
-- ok=0 으로 만들지 않는 이유: 인증서가 30일 뒤에 만료돼도 서비스는
-- 지금 멀쩡히 돌고 있다. 실패로 세면 업타임 통계가 망가진다.
--
-- 00001 을 고치지 않고 새 파일을 추가한 점에 주목.
-- 이미 적용된 마이그레이션은 goose_db_version 에 기록돼 있어서
-- 고쳐도 다시 실행되지 않는다. 남의 DB 는 옛 스키마 그대로 남는다.
ALTER TABLE checks ADD COLUMN warning TEXT NOT NULL DEFAULT '';

-- type 도 함께 남긴다. 한 모니터의 타입이 바뀌었을 때
-- 과거 체크가 어떤 방식으로 이뤄졌는지 알 수 있어야 한다.
ALTER TABLE checks ADD COLUMN type TEXT NOT NULL DEFAULT 'http';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE checks DROP COLUMN type;
ALTER TABLE checks DROP COLUMN warning;
-- +goose StatementEnd
