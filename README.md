# upcheck

셀프호스팅 가용성 모니터링 + 공개 상태 페이지. Go로 만든다.

수백 개 엔드포인트를 동시에 감시하고, 죽으면 알리고, 그 결과를 상태 페이지로 보여준다.
전체 설계는 [go-upcheck.md](go-upcheck.md)에 있다.

> **Go를 배우면서 만드는 프로젝트다.** 단계마다 필요한 Go 개념을
> [`docs/`](docs/README.md) 에 정리해 둔다.

## 진행 상황

- [x] **M0** — 순차 체크 · [기록](docs/05-M0-순차-체크.md)
- [x] **M1** — 중앙 스케줄러 + 워커풀 · [설계 비교](docs/08-M1-동시성.md)
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
go run ./cmd/upcheck                    # 스케줄러 구동 (Ctrl+C 로 종료)
go run ./cmd/upcheck -once              # 순차로 한 번만 체크 (M0 동작)
go run ./cmd/upcheck -scheduler ticker  # 비교용 구현으로 구동
```

주요 플래그:

| 플래그 | 뜻 |
|--------|-----|
| `-config` | 설정 파일 경로 (기본 `configs/monitors.yaml`) |
| `-once` | 순차적으로 한 번만 체크하고 종료 |
| `-scheduler` | `pool`(기본) 또는 `ticker` |
| `-duration` | 이 시간만큼 돌고 자동 종료 (측정용) |
| `-quiet` | 개별 결과를 출력하지 않음 |
| `-dump-goroutines` | 종료 후 남은 goroutine 스택 출력 |

### 스케줄러 두 방식 비교

스펙 M1이 요구한 설계 결정을 **측정으로** 정리했다. 터미널 두 개가 필요하다.

```bash
go run ./cmd/slowserver -delay 200ms                                    # 터미널 1

go run ./cmd/upcheck -config configs/bench-50.yaml -duration 11s -quiet  # 터미널 2
curl -s localhost:8080/stats
```

모니터 50개 · 주기 5초 · 응답 200ms · workers 8 기준:

| | pool | ticker |
|---|---|---|
| 서버가 관측한 최대 동시 요청 | **3** | **50** |
| 과부하 시 최대 goroutine | 29 | 154 |
| 과부하를 보고하는가 | 그렇다 | 아니다 (조용히 버림) |

같은 양의 일을 하면서 상대 서버가 받는 순간 부하는 17분의 1이다.
근거와 구현 과정은 [docs/08-M1-동시성.md](docs/08-M1-동시성.md).

M0의 순차 측정(모니터 50개 = **10.085초**)은
[docs/05-M0-순차-체크.md](docs/05-M0-순차-체크.md).

## 개발

```bash
go build ./...
go test -race ./...
go vet ./...
gofmt -l .
```

## 설정

```yaml
workers: 8              # 동시에 돌릴 워커 수. 동시 HTTP 요청의 상한이다.

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
cmd/slowserver/     테스트용 느린 서버 (동시성 측정 포함)
internal/config/    YAML 파싱 · 기본값 · 검증
internal/checker/   HTTP 체크 한 번
internal/scheduler/ 스케줄러 두 구현 (pool / ticker)
configs/            설정 파일
docs/               Go 학습 노트
```
