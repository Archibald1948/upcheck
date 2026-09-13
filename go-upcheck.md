# upcheck

> 셀프호스팅 가용성 모니터링 + 공개 상태 페이지 — Go

**레포명**: `upcheck` (대안: `pulsewatch`, `sentinela`)
**언어**: Go 1.27 / TypeScript(상태 페이지)
**예상 기간**: 2~3주 (M5까지 기준)

---

## 1. 무엇을 만드는가

수백 개 엔드포인트를 동시에 감시하고, 죽으면 알리고, 그 결과를 공개 상태 페이지로 보여주는 서비스.
Uptime Kuma / Better Stack 같은 도구의 축소판을 직접 만듭니다.

## 2. 왜 의미 있는가

- **만들고 끝나는 프로젝트가 아닙니다.** 본인이 배포한 서비스에 실제로 붙여서 계속 씁니다.
- Go의 goroutine / channel / `context`가 **억지 없이 자연스럽게 필요한** 문제입니다. "동시성 배우려고 만든 예제"가 아니라 진짜로 그게 필요합니다.
- 프론트(Next.js 상태 페이지)가 자연스럽게 붙어서 기존 실력이 그대로 쓰입니다.
- 배포가 쉽습니다. Go는 바이너리 하나로 떨어져서 라즈베리파이든 VPS든 그냥 던져 놓으면 됩니다.

## 3. 기술 스택

| 목적 | 선택 |
|------|------|
| 언어 | Go 1.27 |
| 라우터 | `net/http` (stdlib 라우팅으로 충분) 또는 `chi` |
| DB | SQLite (`modernc.org/sqlite` — cgo 없이 순수 Go) 로 시작, 필요 시 Postgres(`pgx`) |
| 마이그레이션 | `pressly/goose` |
| 로깅 | `log/slog` (stdlib 구조적 로깅) |
| 설정 | YAML (`gopkg.in/yaml.v3`) |
| 메트릭 | `prometheus/client_golang` |
| 프론트 | Next.js App Router |

> Go 1.27에 **`goroutineleak` 프로파일**이 새로 들어왔습니다 (`/debug/pprof/goroutineleak`). 이 프로젝트는 goroutine 누수가 나기 딱 좋은 구조라, 이걸 실제로 돌려보는 게 훌륭한 학습 소재입니다.

## 4. 마일스톤

### M0 — 순차 체크
YAML 설정에서 모니터 목록을 읽어 **순차적으로** HTTP GET → 콘솔 출력.
일부러 느리게 만들어 놓고, 모니터 50개면 얼마나 걸리는지 재보세요. M1의 동기가 됩니다.

### M1 — 동시성
- 중앙 스케줄러가 "지금 체크할 때가 된 모니터"를 큐에 넣음
- 워커풀(고정 개수 goroutine)이 큐에서 꺼내 실행
- 개별 체크마다 `context.WithTimeout`
- 결과는 채널로 팬인(fan-in)해서 한 곳에서 수집

**설계 결정 지점**: 모니터마다 `time.Ticker`를 하나씩 두는 방식 vs 중앙 스케줄러 + 워커풀.
전자는 코드가 짧고 후자는 확장성이 좋습니다. 둘 다 짜보고 왜 후자를 골랐는지 README에 쓰세요.

### M2 — 저장 & 집계
```sql
monitors(id, name, type, target, interval_sec, timeout_ms,
         expected_status, keyword, enabled, created_at)
checks(id, monitor_id, checked_at, ok, status_code, latency_ms, error)
incidents(id, monitor_id, started_at, resolved_at, cause)
```
계산할 것: 업타임 % (24h / 7d / 30d), 응답시간 p50 / p95, 현재 상태.

`checks` 테이블은 금방 커집니다. 모니터 100개 × 60초 주기 = 하루 14만 행.
→ **시간별 롤업 테이블(`checks_hourly`)을 만들고 오래된 raw 데이터는 삭제하는 정리 잡**을 넣으세요. 이걸 구현하면 실무 감각이 확 붙습니다.

### M3 — 알림
- **상태 전이에만** 발송 (up→down, down→up). 매 체크마다 보내면 안 됩니다.
- **플래핑 방지**: 연속 N회 실패해야 down으로 확정 (기본 3회)
- **쿨다운**: 같은 모니터에 대해 X분 내 재발송 금지
- 채널: Discord 웹훅, Slack 웹훅 (인터페이스로 추상화해서 나중에 추가 가능하게)

```go
type Notifier interface {
    Notify(ctx context.Context, ev Event) error
    Name() string
}
```

### M4 — 체크 타입 확장
| 타입 | 검사 내용 |
|------|----------|
| `http` | 상태 코드, 응답 본문 키워드 매칭, 리다이렉트 정책 |
| `tcp` | 포트 연결 가능 여부 |
| `tls` | 인증서 만료까지 남은 일수 (30일 이하면 경고) |
| `dns` | 특정 레코드가 기대값으로 해석되는지 |

### M5 — HTTP API + 상태 페이지
- `GET /api/status` — 전체 현황
- `GET /api/monitors/:id/history?days=90`
- Next.js: 90일 업타임 바(Statuspage 스타일), 인시던트 타임라인, 응답시간 그래프

### M6 — 운영 준비
graceful shutdown, `/healthz`, `/metrics`(Prometheus), `/debug/pprof`, Dockerfile(멀티스테이지, distroless), docker-compose.

## 5. 동시성 설계 — 이 프로젝트의 핵심

```go
// 개념도
rootCtx, cancel := context.WithCancel(context.Background())
// SIGINT/SIGTERM → cancel()

scheduler → jobCh (chan Job, buffered)
                ↓
        worker × N (errgroup)
                ↓
           resultCh (chan Result)
                ↓
        collector → DB + 상태 전이 판정 → notifyCh
```

**반드시 지킬 것**

1. 모든 goroutine이 `rootCtx.Done()`을 감시하고 빠져나올 경로가 있을 것
2. 채널을 닫는 주체를 명확히 (보내는 쪽이 닫는다)
3. 워커 수를 무제한으로 두지 말 것 — 모니터 1000개면 goroutine 1000개가 동시에 HTTP를 때립니다
4. `go test -race`를 CI에 넣을 것

## 6. 완료 기준

- [ ] 100개 모니터 등록 후 1시간 구동 시 goroutine 수가 증가하지 않음 (`/debug/pprof/goroutine` 및 Go 1.27 `goroutineleak` 프로파일로 확인)
- [ ] 일부러 죽인 엔드포인트에 대해 down 알림이 **정확히 1회** 발송되고, 복구 시 up 알림 1회
- [ ] 3초에 한 번 껐다 켜지는(플래핑) 엔드포인트에 알림 폭탄이 발생하지 않음
- [ ] `Ctrl+C` 시 진행 중인 체크를 정리하고 5초 내 종료
- [ ] `go test -race ./...` 통과
- [ ] 상태 페이지에서 90일 업타임 바가 렌더됨
- [ ] `docker compose up` 한 방으로 뜸

## 7. 함정 목록

| 함정 | 대응 |
|------|------|
| 매 체크마다 `&http.Client{}` 새로 생성 | 클라이언트 하나를 재사용. `Transport`의 `MaxIdleConnsPerHost`, `IdleConnTimeout` 튜닝 |
| 응답 본문을 안 닫음 | `defer resp.Body.Close()` + 키워드 매칭 안 해도 `io.Copy(io.Discard, resp.Body)`로 비워야 커넥션 재사용됨 |
| 타임아웃이 `http.Client.Timeout`에만 걸림 | `http.NewRequestWithContext`로 context 타임아웃을 써야 셧다운 시 즉시 취소됨 |
| 로컬 시간대로 저장 | 전부 UTC로 저장, 표시할 때만 변환 |
| SQLite 동시 쓰기 잠금 | WAL 모드 활성화, 쓰기는 한 goroutine으로 직렬화 |
| 알림 발송이 체크 루프를 블로킹 | 별도 goroutine + 버퍼 채널. 알림 서버가 느려도 모니터링은 계속돼야 함 |

## 8. Claude Code 첫 지시 예시

```
이 스펙(03-go-upcheck.md)의 M0~M2까지 구현해줘.

- YAML 설정에서 모니터 목록을 읽는다
- 중앙 스케줄러 + 워커풀(개수 설정 가능)로 동시 체크. 개별 체크는 context 타임아웃.
- 결과를 SQLite(modernc.org/sqlite)에 저장하고, 24h 업타임 %와 p95 응답시간을 계산.
- graceful shutdown: SIGINT 시 root context를 cancel하고 진행 중 체크가 정리되는지
  검증하는 테스트 포함.
- go test -race ./... 통과 필수.

M3(알림) 이후는 아직 하지 마.
스케줄러를 "모니터당 Ticker" 대신 "중앙 스케줄러 + 워커풀"로 짠 이유를 주석으로 남겨줘.
```

## 9. 학습 포인트 (여기서 물어보세요)

- `context`가 goroutine 트리를 타고 취소를 전파하는 정확한 메커니즘
- 버퍼 채널과 언버퍼 채널의 선택 기준, 버퍼 크기는 어떻게 정하는가
- `sync.WaitGroup` vs `errgroup.Group` — 언제 무엇을
- goroutine 누수가 발생하는 전형적인 패턴 4가지와 각각의 탐지법
- `http.Transport` 커넥션 풀이 실제로 어떻게 동작하는가
