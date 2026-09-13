# upcheck

셀프호스팅 가용성 모니터링 + 공개 상태 페이지. Go로 만든다.

수백 개 엔드포인트를 동시에 감시하고, 죽으면 알리고, 그 결과를 상태 페이지로 보여준다.
전체 설계는 [go-upcheck.md](go-upcheck.md)에 있다.

> **Go를 배우면서 만드는 프로젝트다.** 단계마다 필요한 Go 개념을
> [`docs/`](docs/README.md) 에 정리해 둔다.

## 진행 상황

- [x] **M0** — 순차 체크 · [기록](docs/05-M0-순차-체크.md)
- [ ] **M1** — 중앙 스케줄러 + 워커풀
- [ ] **M2** — SQLite 저장 & 집계
- [ ] **M3** — 알림 (플래핑 방지, 쿨다운)
- [ ] **M4** — 체크 타입 확장 (tcp / tls / dns)
- [ ] **M5** — HTTP API + Next.js 상태 페이지
- [ ] **M6** — 운영 준비 (graceful shutdown, metrics, Docker)

## 요구 사항

Go 1.27 이상.

```bash
brew install go
```

## 실행

```bash
go run ./cmd/upcheck                              # configs/monitors.yaml 사용
go run ./cmd/upcheck -config configs/bench-50.yaml
```

### 벤치마크

순차 체크가 얼마나 느린지 직접 재본다. 터미널 두 개가 필요하다.

```bash
go run ./cmd/slowserver -delay 200ms              # 터미널 1
go run ./cmd/upcheck -config configs/bench-50.yaml # 터미널 2
```

현재(M0): 모니터 50개 × 200ms = **10.085초**. `interval_sec: 10` 을 못 지킨다.
자세한 분석은 [docs/05-M0-순차-체크.md](docs/05-M0-순차-체크.md).

## 개발

```bash
go build ./...
go test -race ./...
go vet ./...
gofmt -l .
```

## 설정

```yaml
workers: 8              # 동시 실행 워커 수 (M1부터 사용)

monitors:
  - name: Google        # 필수
    type: http          # 기본값 http (M4에서 tcp/tls/dns 추가)
    target: https://www.google.com   # 필수
    interval_sec: 60    # 기본값 60
    timeout_ms: 5000    # 기본값 5000
    expected_status: 200 # 기본값 200
    keyword: 정상        # 선택 — 응답 본문에 이 문자열이 있어야 UP
    enabled: true       # 기본값 true
```

## 구조

```
cmd/upcheck/        본체
cmd/slowserver/     테스트용 느린 서버
internal/config/    YAML 파싱 · 기본값 · 검증
internal/checker/   HTTP 체크 한 번
configs/            설정 파일
docs/               Go 학습 노트
```
