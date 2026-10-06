-- +goose Up
-- +goose StatementBegin

-- warning 은 "실패는 아니지만 알아둘 것"이다. 지금은 tls 인증서 만료 임박에 쓴다.
ALTER TABLE checks ADD COLUMN warning TEXT NOT NULL DEFAULT '';

-- 과거 체크가 어떤 타입으로 이뤄졌는지 남긴다.
ALTER TABLE checks ADD COLUMN type TEXT NOT NULL DEFAULT 'http';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE checks DROP COLUMN type;
ALTER TABLE checks DROP COLUMN warning;
-- +goose StatementEnd
