# upcheck

셀프호스팅 가용성 모니터링 + 공개 상태 페이지. Go로 만든다.

수백 개 엔드포인트를 동시에 감시하고, 죽으면 알리고, 그 결과를 상태 페이지로 보여준다.
전체 설계는 [go-upcheck.md](go-upcheck.md)에 있다.

> **Go를 배우면서 만드는 프로젝트다.** 단계마다 필요한 Go 개념을
> [`docs/`](docs/README.md) 에 정리해 둔다.

## 진행 상황

- [x] **M0** — 순차 체크 · [기록](docs/05-M0-순차-체크.md)
- [x] **M1** — 중앙 스케줄러 + 워커풀 · [설계 비교](docs/08-M1-동시성.md)
- [x] **M2** — SQLite 저장 & 집계 · [기록](docs/10-M2-저장과집계.md)
- [x] **M3** — 알림 (플래핑 방지, 쿨다운) · [기록](docs/11-M3-알림.md)
- [x] **M4** — 체크 타입 확장 (tcp / tls / dns) · [기록](docs/12-M4-체크타입.md)
- [x] **M5** — HTTP API + Next.js 상태 페이지 · [기록](docs/14-M5-API와-상태페이지.md)
- [x] **M6** — 운영 준비 (graceful shutdown, metrics, Docker) · [기록](docs/16-M6-운영준비.md)

## 요구 사항

- Go 1.27 이상
- 상태 페이지를 띄우려면 Node.js 20.9 이상

```bash
brew install go
```

## 도커로 한 번에 띄우기

```bash
docker compose up --build     # → http://localhost:3000
```

| 포트 | 무엇 | 노출 |
|---|---|---|
| 3000 | 상태 페이지 | 호스트에 공개 |
| 8484 | 공개 API | compose 내부만 |
| 8485 | 운영 (metrics · healthz · pprof) | compose 내부만 |

pprof 는 힙 내용과 goroutine 스택을, 지표는 내부 구조를 드러낸다.
공개 API 와 같은 포트에 두지 않는다.

이미지는 upcheck 27MB(distroless), 상태 페이지 350MB.

## 상태 페이지 한 번에 보기

90일치 데모 데이터로 상태 페이지를 바로 띄울 수 있다. 터미널 두 개가 필요하다.

```bash
go run ./cmd/seeddemo -db demo.db              # 90일 × 6개 모니터 데모 데이터
go run ./cmd/upcheck -db demo.db -api-only     # 터미널 1: API 만 (:8484)

cd web && npm install && npm run dev           # 터미널 2: http://localhost:3000
```

실제로 감시하면서 띄우려면 터미널 1 에서 `go run ./cmd/upcheck` 를 쓴다.
체크하면서 같은 프로세스에서 API 를 함께 연다.

```
브라우저 ──> Next.js 서버 (web/) ──> upcheck API (:8484) ──> SQLite
```

브라우저는 Go API 를 직접 부르지 않는다. Next.js 서버 컴포넌트만 부르므로
CORS 설정이 필요 없고 Go API 를 외부에 열 필요도 없다.

| 화면 | 내용 |
|---|---|
| `/` | 전체 상태 배너 · 모니터별 90일 업타임 바 · 최근 장애 |
| `/monitors/{id}` | 가용성·응답시간 타일 · 업타임 바 · 일별 응답시간 차트 · 장애 이력 |

설계와 시행착오는 [docs/14-M5-API와-상태페이지.md](docs/14-M5-API와-상태페이지.md).

## 실행

```bash
go run ./cmd/upcheck                    # 체크하면서 SQLite 에 쌓는다 (Ctrl+C 로 종료)
go run ./cmd/upcheck -report            # 체크 없이 누적 현황만 출력
go run ./cmd/upcheck -once              # 순차로 한 번만 체크 (M0 동작)
go run ./cmd/upcheck -scheduler ticker  # 비교용 구현으로 구동
```

`-report` 출력:

```
모니터 6개 · 보관 중인 원본 체크 1842행 · 2026-09-13 12:01:09 기준

상태   모니터                          24h        7d       30d       p50       p95
──────────────────────────────────────────────────────────────────────────────
 UP  Cloudflare              100.00%   100.00%   100.00%     542ms     611ms
 UP  GitHub                  100.00%   100.00%   100.00%     112ms     147ms
DOWN 404 나는 주소                 0.00%     0.00%     0.00%         -         -
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
| `-db` | SQLite 파일 경로 (기본 `upcheck.db`) |
| `-http` | API 서버 주소 (기본 `:8484`, 빈 값이면 끔) |
| `-api-only` | 체크 없이 저장된 데이터로 API 만 제공 |
| `-expose-details` | API 응답에 target 주소·에러 원문·경고 포함 (기본 숨김) |
| `-admin` | 운영 서버 주소 (기본 `127.0.0.1:8485`, 빈 값이면 끔) |
| `-healthcheck` | 준비 상태를 확인하고 종료 (도커 HEALTHCHECK 용) |

## 운영

```bash
curl http://127.0.0.1:8485/healthz    # liveness — 프로세스가 응답하는가
curl http://127.0.0.1:8485/readyz     # readiness — DB까지 확인
curl http://127.0.0.1:8485/metrics    # Prometheus 지표

# goroutine 누수 확인 (Go 1.27 신규 프로파일)
curl "http://127.0.0.1:8485/debug/pprof/goroutineleak?debug=1"
go tool pprof -http=: http://127.0.0.1:8485/debug/pprof/heap
```

모니터 100개를 150초 구동하며 `go_goroutines` 를 재면 39개에서 평평하고
`goroutineleak` 은 0이다. `docker compose stop` 은 1.44초에 끝난다.
근거는 [docs/16-M6-운영준비.md](docs/16-M6-운영준비.md).
| `-report` | 체크하지 않고 저장된 현황만 출력 |
| `-maintain-every` | 롤업·정리 잡 주기 (기본 10분) |

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

### 롤업이 데이터를 얼마나 줄이나

```bash
go test ./internal/store/ -run TestRollupAtScale -v
```

```
모니터 100개 × 1m0s 주기 × 24h0m0s
  원본      :  144000행 ·   7.5 MB
  롤업      :    2400행 · 소요 234ms
  정리 후   :    2400행 ·   0.1 MB
  → 행 60분의 1, 용량 55분의 1
```

업타임 %는 개수 합이라 롤업 후에도 **정확**하고, 응답시간 백분위수만
근사가 된다(값에 `Approx` 로 표시). 설계 근거는
[docs/10-M2-저장과집계.md](docs/10-M2-저장과집계.md).

## 개발

```bash
go build ./...
go test -race ./...
go vet ./...
gofmt -l .
```

## 설정

```yaml
workers: 8              # 동시에 돌릴 워커 수. 동시 요청의 상한이다.

monitors:
  # http — 상태 코드 · 본문 키워드
  - name: Google        # 필수
    type: http          # 기본값 http
    target: https://www.google.com   # 스킴 필수
    interval_sec: 60    # 기본값 60
    timeout_ms: 5000    # 기본값 5000
    expected_status: 200 # 기본값 200
    keyword: 정상        # 선택 — 응답 본문에 이 문자열이 있어야 UP
    enabled: true       # 기본값 true

  # tcp — 포트가 열려 있는지만 확인 (DB, 큐, SMTP 등)
  - name: PostgreSQL
    type: tcp
    target: db.example:5432   # 포트 필수

  # tls — 인증서 만료까지 남은 일수
  - name: 인증서
    type: tls
    target: example.com       # 포트 생략 시 443
    cert_warn_days: 30        # 기본값 30. 이하로 남으면 경고

  # dns — 레코드가 기대값으로 해석되는지
  - name: DNS 레코드
    type: dns
    target: example.com       # 호스트 이름만
    record: A                 # A, AAAA, CNAME, TXT, MX, NS (기본 A)
    expect: [1.2.3.4]         # 하나라도 나오면 정상. 비우면 해석만 확인
    resolver: 8.8.8.8:53      # 선택 — 비우면 시스템 기본값

alerts:
  failure_threshold: 3  # 연속 3회 실패해야 down 확정 (플래핑 1차 방어)
  success_threshold: 1  # 연속 1회 성공하면 up 확정
  cooldown: 5m          # 같은 모니터 재발송 금지 간격 (플래핑 2차 방어)
  # discord_webhook: ${UPCHECK_DISCORD_WEBHOOK}
  # slack_webhook: ${UPCHECK_SLACK_WEBHOOK}
```

웹훅 URL 자체가 인증 수단이다. 설정 파일에는 `${VAR}` 참조만 적고
실제 주소는 환경변수로 넘긴다.

```bash
export UPCHECK_DISCORD_WEBHOOK="https://discord.com/api/webhooks/..."
go run ./cmd/upcheck
```

웹훅을 지정하지 않아도 판정과 장애 이력 기록은 그대로 돌아간다.

### 체크 타입

| 타입 | 검사 내용 | target 형식 |
|------|-----------|-------------|
| `http` | 상태 코드 · 본문 키워드 · 리다이렉트 | `https://example.com/health` |
| `tcp` | 포트 연결 가능 여부 | `db.example:5432` |
| `tls` | 인증서 만료까지 남은 일수 | `example.com` (기본 443) |
| `dns` | 레코드가 기대값으로 해석되는지 | `example.com` |

```
[UP  ] http  Google              270ms  200
[UP  ] tcp   Google DNS (TCP)     67ms  연결됨
[UP  ] tls   GitHub 인증서          17ms  만료 D-77 (2026-11-30)
[UP  ] dns   Google DNS 레코드       1ms  A 8.8.4.4 8.8.8.8
```

인증서 만료 임박은 **경고**이지 실패가 아니다. 30일 뒤에 만료돼도 서비스는
지금 멀쩡히 돌고 있어서, 실패로 세면 업타임이 망가지고 가짜 장애 알림이 간다.
`-report` 에서 따로 모아 보여준다. 근거는 [docs/12-M4-체크타입.md](docs/12-M4-체크타입.md).

### 알림 규칙

| 관문 | 하는 일 |
|------|---------|
| 임계치 | 연속 N회 실패해야 down 확정 — 일시적 끊김으로 깨우지 않는다 |
| 상태 전이 | 이미 알린 상태와 같으면 안 보낸다 — 매 체크마다 보내지 않는다 |
| 쿨다운 | X분 내 재발송 금지 — 억제된 알림은 잃지 않고 **미룬다** |

실측: 3초 주기로 껐다 켜지는 엔드포인트를 60초 감시 → 알림 **3건**(쿨다운 20초 기준),
29건 억제. 서버를 죽였다 살리면 down 1회 + up 1회. 근거는
[docs/11-M3-알림.md](docs/11-M3-알림.md).

## 구조

```
cmd/upcheck/        본체
cmd/slowserver/     테스트용 느린 서버 (동시성 측정 포함)
cmd/seeddemo/       90일 데모 데이터 생성기
internal/config/    YAML 파싱 · 기본값 · 검증
internal/checker/   체크 타입별 구현 (http / tcp / tls / dns)
internal/scheduler/ 스케줄러 두 구현 (pool / ticker)
internal/store/     SQLite 저장 · 롤업 · 집계 · 장애 이력
internal/collector/ 결과를 모아 배치로 저장
internal/alert/     상태 전이 판정 · 알림 발송 (Discord / Slack)
internal/api/       상태 페이지용 HTTP JSON API
internal/admin/     운영 엔드포인트 (healthz · metrics · pprof)
internal/metrics/   Prometheus 지표
configs/            설정 파일
web/                Next.js 상태 페이지
docs/               Go 학습 노트
Dockerfile          upcheck 이미지 (멀티스테이지 → distroless)
docker-compose.yml  전체 스택
```
