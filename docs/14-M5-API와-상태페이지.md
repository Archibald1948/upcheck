# 14 — M5: HTTP API 와 상태 페이지

## 스펙이 요구한 것

> - `GET /api/status` — 전체 현황
> - `GET /api/monitors/:id/history?days=90`
> - Next.js: 90일 업타임 바(Statuspage 스타일), 인시던트 타임라인, 응답시간 그래프
>
> 완료 기준: 상태 페이지에서 90일 업타임 바가 렌더됨
> — `go-upcheck.md` M5, 6절

## 구조

```
브라우저 ──> Next.js 서버 (web/) ──> upcheck Go API (:8484) ──> SQLite
             서버 컴포넌트가           internal/api
             렌더링해서 HTML 로
```

**브라우저는 Go API 를 직접 부르지 않는다.** Next.js 서버 컴포넌트만 부른다.

- CORS 설정이 필요 없다
- Go API 를 인터넷에 열 필요가 없다. 내부망에만 두면 된다
- API 주소가 JS 번들에 박히지 않는다 (`NEXT_PUBLIC_` 을 안 쓴다)

## 써 보기

```bash
# 1. 90일치 데모 데이터
go run ./cmd/seeddemo -db demo.db

# 2. API 만 띄우기 (체크 없이 DB 를 읽는다)
go run ./cmd/upcheck -db demo.db -api-only

# 3. 상태 페이지
cd web && npm install && npm run dev      # http://localhost:3000
```

실제로 감시하면서 띄우려면 2 대신 `go run ./cmd/upcheck` 를 쓴다.
데몬이 체크하면서 같은 프로세스에서 API 를 함께 연다.

## API

| 엔드포인트 | 내용 |
|---|---|
| `GET /api/status` | 전체 상태 한 줄(`operational`/`degraded`/`outage`) + 모니터별 현재 상태, 24h/7d/30d 업타임, 24h p50/p95 |
| `GET /api/monitors/{id}/history?days=90&tz=Asia/Seoul` | 일별 업타임·응답시간 + 장애 이력 |
| `GET /api/incidents?limit=20` | 전체 모니터의 최근 장애 |

스펙의 두 개에 `/api/incidents` 를 더했다. 홈 화면의 인시던트 타임라인이
모든 모니터를 가로질러야 해서다.

### 설계 결정

**1. 내부 정보는 기본으로 숨긴다**

상태 페이지는 공개용이다. 그런데 에러 원문은 이렇게 생겼다.

```
dial tcp 10.0.3.17:5432: connect: connection refused
```

내부 IP, 포트, 쓰는 DB 종류(5432 = PostgreSQL)가 다 들어 있다.
target 주소, 에러 원문, 인증서 경고는 `-expose-details` 를 켜야만 응답에 실린다.
500 응답에도 에러 원문을 싣지 않고 서버 로그에만 남긴다.

`TestDetailsHiddenByDefault` 가 세 엔드포인트 응답 문자열 어디에도
내부 정보가 없는지 **직접** 검사한다.

**2. 데이터가 없으면 null**

```json
{ "uptime": { "24h": null }, "last_checked_at": null }
```

0% 업타임은 "완전히 죽어 있었다"는 뜻이다. 한 번도 체크 안 한 모니터를
0% 로 보내면 거짓말이다. 업타임 바에서도 기록 없는 날은 빨강이 아니라 **회색**이다.

**3. 날짜 경계는 요청한 시간대의 자정**

```
GET /api/monitors/1/history?tz=Asia/Seoul
```

UTC 로 자르면 한국에서는 **오전 9시에 날이 바뀐다.** 새벽 3시 장애가
"어제" 칸에 찍힌다. 시간대를 받아 그 기준 자정으로 자른다.
`TestDailyHistoryRespectsTimezone` 이 같은 체크가 UTC 로는 9일,
서울로는 10일 칸에 들어가는지 확인한다.

날짜는 `24 * time.Hour` 를 더하지 않고 `AddDate(0, 0, 1)` 로 만든다.
서머타임이 있는 시간대는 하루가 23시간이나 25시간일 수 있다.

## 상태 페이지 (web/)

Next.js 16 · App Router · 서버 컴포넌트. Tailwind 없이 CSS Modules.

```
web/src/
├── lib/
│   ├── api.ts        Go API 클라이언트 (server-only)
│   ├── types.ts      API 응답 타입 (Go 쪽과 맞춰 둔다)
│   ├── status.ts     업타임 → 등급, 기간 가용성
│   └── format.ts     날짜·숫자 표시
├── components/
│   ├── UptimeBar.tsx          90일 업타임 바
│   ├── ResponseTimeChart.tsx  일별 응답시간
│   ├── IncidentList.tsx       장애 타임라인
│   ├── StatusIcon.tsx         상태 아이콘
│   └── AutoRefresh.tsx        60초마다 갱신
└── app/
    ├── page.tsx               /
    └── monitors/[id]/page.tsx /monitors/{id}
```

### 빌드 때 상태가 굳지 않게

Next.js 16 은 Cache Components 를 켜지 않으면, 요청 시점 API 앞에 있는
`fetch` 를 **`next build` 때 한 번 실행**해서 페이지를 정적으로 만든다.

- 상태 페이지가 **빌드한 순간의 상태로 굳는다**
- 빌드할 때 Go API 가 안 떠 있으면 **빌드가 실패한다**

그래서 API 를 부르기 전에 `connection()` 을 부른다.

```ts
async function getJSON<T>(path: string): Promise<T> {
  await connection();   // 여기서 프리렌더링이 멈춘다
  const res = await fetch(`${API_URL}${path}`, { cache: "no-store", ... });
}
```

Go API 를 끈 상태에서 빌드해서 확인했다.

```
Route (app)
┌ ƒ /
├ ○ /_not-found
└ ƒ /monitors/[id]

ƒ  (Dynamic)  server-rendered on demand
```

> 이 동작은 버전마다 바뀌어 왔다(Next 14 는 기본 캐시, 15 는 기본 비캐시,
> 16 은 Cache Components 도입). 스캐폴드의 `AGENTS.md` 가 "학습 데이터와 다른
> Next.js 이니 `node_modules/next/dist/docs/` 부터 읽으라"고 하는 이유다.
> 실제로 읽고 나서 구조를 정했다.

### 차트 설계

dataviz 원칙을 따랐다. 요점만 남긴다.

**90일 업타임 바**

| 등급 | 기준 | 색 |
|---|---|---|
| 정상 | 99.9% 이상 | good |
| 경미한 실패 | 99% 이상 | warning |
| 부분 장애 | 95% 이상 | serious |
| 주요 장애 | 95% 미만 | critical |
| 기록 없음 | — | 회색 |

- 칸 사이는 **테두리가 아니라 2px 표면색 간격**으로 나눈다
- 상태 색은 절대 혼자 뜻을 전하지 않는다. 모양이 다른 **아이콘 + 라벨 + 기준값**을 붙인다
- 칸 90개를 Tab 으로 하나씩 지나가지 않는다. 바 전체가 Tab 한 번이고
  안에서 ←/→/Home/End 로 움직인다(roving tabindex)
- 툴팁은 **값이 먼저**, 라벨·날짜가 뒤. 같은 값을 표 보기로도 읽을 수 있다
- 640px 이하에서는 최근 30일만. 400px 폭에 90칸이면 칸이 2px 남짓이다

99.9% 기준은 엄격하다. 하루 288번 체크 중 **한 번만** 실패해도 99.65% 라
노란 칸이 된다. 상태 페이지의 업타임은 "장애가 없던 시간"이 아니라
"체크 성공률"이기 때문이다. 판정 엔진은 연속 3회 실패해야 장애로 보지만,
업타임은 그와 무관하게 모든 체크를 센다.

**응답시간 차트**

- p50 과 p95 는 단위가 같아 **축 하나**에 둔다
- 시리즈 색은 검증 스크립트로 라이트/다크 모두 통과시켰다
  (색각 이상 시뮬레이션 ΔE 24.7 / 26.8, 기준 8 이상)
- 범례는 항상 두고, 끝점에 직접 라벨을 단다
- 기록 없는 날에서는 **선을 끊는다**. 이어 그리면 없는 값을 지어내는 셈이다
- 크로스헤어가 가장 가까운 날짜에 붙고, 툴팁 하나에 두 시리즈를 다 보여준다
- 표 보기를 함께 둔다

## 데모 데이터 — 파이프라인 통합 테스트를 겸한다

90일 업타임 바를 보려면 90일치 데이터가 필요하다. `cmd/seeddemo` 가
과거 시각으로 체크 결과를 만든다. 핵심은 **DB 에 직접 INSERT 하지 않는다**는 것이다.

```
체크 결과 → store.InsertChecks
          → alert.Engine.Observe   (장애 이력은 판정 엔진이 기록한다)
          → store.Rollup → store.Prune
```

```
90일 × 6개 모니터 × 5m0s 간격 데모 데이터 생성
  체크 155526행 저장 · 650ms
  롤업 12960칸 · 원본 143424행 정리 (7일 보관) · 959ms
  장애 이력 9건 (진행 중 1건) — 판정 엔진이 기록
```

설계한 장애 9건(웹사이트 2, API 2, 결제 플래핑 3, 워커 2)을
판정 엔진이 **정확히 9건** 기록했고, 무작위 단발 실패는 연속 3회 임계치 때문에
하나도 장애가 되지 않았다. M3의 판정 규칙이 90일 규모에서 맞게 동작한다는 뜻이다.

그리고 이 도구로 **본체 버그 두 개**를 찾았다. 아래.

## 부딪힌 것들

### 1. 24시간 응답시간이 항상 근사였다

데모 데이터로 API 를 찍었더니 모든 모니터가 `approx: true` 였다.

M2 에서는 "롤업 경계보다 앞 구간이 섞이면 근사"로 판정했다. 그런데 롤업은
원본을 **지우지 않고** 접은 사본을 만들 뿐이다. 원본은 정리 잡이 지우기 전까지(7일)
그대로 있다. 롤업이 10분마다 도는 데몬에서는 **24시간 p95 가 영원히 근사**였던 것이다.

원본이 구간을 빠짐없이 덮는지 직접 확인하도록 바꿨다.

```sql
-- since 이후이면서 원본의 가장 이른 시각보다 앞선 롤업 칸이 있는가?
SELECT COUNT(*) FROM checks_hourly
WHERE monitor_id = ? AND hour < :minRaw AND hour + 3600 > :since
```

칸의 **시작**이 아니라 **끝**(`hour + 3600`)과 비교하는 게 중요하다.
정리 기준이 `now - 7일` 이라 원본이 정시가 아닌 시각(예: 10:37)부터 남아 있을 수 있다.
그러면 10시 칸은 10:00~10:37 을 원본 없이 들고 있다.

### 2. 응답시간 가중치가 틀렸다

일별 이력을 짜다가 M2 코드를 다시 보니, 롤업 칸의 p50/p95 는 **성공한 체크만**으로
계산하는데 칸을 합칠 때는 **실패 포함 전체 개수**로 가중하고 있었다.

```
10시: 60번 성공, 100ms     → 무게 60
11시: 59번 실패 + 1번 성공, 1000ms → 무게 60  ← 표본 1개짜리가 60표
```

고치기 전 p50 **550ms**, 고친 후 **114ms**. 실패 테스트를 먼저 쓰고 고쳤다.

### 3. POST 가 405 가 아니라 404 였다

JSON 404 용 catch-all 을 `"/"` 로 걸었더니 모든 메서드를 삼켰다.
`"GET /"` 로 바꿔 해결했다. 자세한 건 [13](13-HTTP-서버.md).

### 4. goroutine 을 띄운 뒤에 에러로 반환할 수 있었다

알림 발송 goroutine 을 먼저 띄우고 장애 복원·포트 잡기를 뒤에서 하고 있었다.
뒤에서 실패해 반환하면 그 goroutine 은 닫히지 않는 채널을 영원히 기다린다.
실패할 수 있는 준비를 전부 앞으로 옮겼다. 자세한 건 [13](13-HTTP-서버.md).

### 5. 시간대를 두 군데서 읽었다

처음엔 API 에 넘길 시간대는 런타임 환경변수(`UPCHECK_TIMEZONE`)로,
화면에 찍을 시간대는 빌드 타임 환경변수(`NEXT_PUBLIC_...`)로 따로 읽게 만들었다.
둘이 어긋나면 **날짜 경계와 표시 시각이 달라진다.**

시각 포맷을 서버 컴포넌트에서만 하도록 바꿔서 시간대를 한 곳에서만 읽는다.
클라이언트 컴포넌트는 `"2026-09-14"` 같은 **달력 날짜**만 다루는데,
이건 시간대와 무관하게 UTC 로 해석하면 된다.

### 6. 화면을 띄워 보고서야 보인 것들

코드와 HTML 검사로는 통과했지만 실제로 렌더링해서 보니 문제가 있었다.

- **좁은 화면에서 바는 30일인데 옆 숫자는 90일 값**이었다. 라벨이 없어서
  30일 값으로 읽힌다. 숫자 앞에 `90일` 을 명시했다
- x축 날짜의 ko-KR 짧은 형식 `6. 17.` 이 좁은 축에서 지저분했다 → `6/17`
- 상세 페이지 장애 이력이 **초록 체크 옆에 "장애"** 라고 적혀 뜻이 엇갈렸다
  → "해소된 장애" / "진행 중인 장애"
- 모바일 2열 타일에서 다섯 번째 타일이 반쪽으로 남았다 → 두 칸으로

그리고 한 가지는 버그가 아니었다. API 서버의 노란 칸이 기대보다 많아 보여서
데이터를 확인했는데, 모니터 넷의 단발 실패일 합계가 관측 24 대 기대 20.7 이었다.
모니터별로는 흔들리지만 전체로는 난수 분산 범위다. **추측하지 않고 세어 본** 경우다.

## 스펙 요구사항 점검

| 요구 | 상태 |
|---|---|
| `GET /api/status` | 전체 상태 + 모니터별 현황 |
| `GET /api/monitors/:id/history?days=90` | 일별 업타임·응답시간·장애, 시간대 지정 가능 |
| 90일 업타임 바 (Statuspage 스타일) | `UptimeBar` — 데모 데이터로 6개 모니터 × 90칸 렌더 확인 |
| 인시던트 타임라인 | `IncidentList` — 진행 중 / 해소 구분 |
| 응답시간 그래프 | `ResponseTimeChart` — 일별 p50/p95 |
| **완료 기준: 90일 업타임 바가 렌더됨** | 실제 브라우저로 데스크톱·390px·라이트/다크 확인 |
| `go test -race ./...` | 통과 |

## 이 단계에서 배운 Go

| 개념 | 어디서 |
|---|---|
| `http.Handler` 인터페이스와 `HandlerFunc` | `server.go` → [13](13-HTTP-서버.md) |
| 함수 타입에 메서드 붙이기 | `HandlerFunc` → [13](13-HTTP-서버.md) |
| 메서드 값 (`s.handleStatus`) | `Handler()` → [13](13-HTTP-서버.md) |
| Go 1.22 ServeMux 패턴, `PathValue` | `Handler()` → [13](13-HTTP-서버.md) |
| 미들웨어 `func(Handler) Handler` | `logRequests` → [13](13-HTTP-서버.md) |
| 인터페이스 임베딩으로 감싸기 | `statusRecorder` → [13](13-HTTP-서버.md) |
| JSON 태그, `omitempty`, 포인터로 null | `types.go` → [13](13-HTTP-서버.md) |
| 탈출 분석 (지역 변수 주소 반환) | `uptimePtr` → [13](13-HTTP-서버.md) |
| `http.Server` 타임아웃과 Slowloris | `Serve` → [13](13-HTTP-서버.md) |
| `Shutdown` 과 `ErrServerClosed` | `Serve` → [13](13-HTTP-서버.md) |
| `httptest.NewRecorder` | `server_test.go` → [13](13-HTTP-서버.md) |
| 제네릭 함수 `decode[T any]` | `server_test.go` → [13](13-HTTP-서버.md) |
| `time.LoadLocation`, `AddDate` | `history.go` |
| `math/rand/v2` 와 고정 시드 | `cmd/seeddemo` |
| 클로저가 바깥 변수를 고치기 | `rawHoursInRange` 의 `flush` |

## 다음: M6

운영 준비다. graceful shutdown, `/healthz`, `/metrics`(Prometheus), `/debug/pprof`,
Dockerfile(멀티스테이지, distroless), docker-compose.

API 서버의 Shutdown 은 이미 들어가 있다. `/healthz` 와 `/metrics` 는 같은
`ServeMux` 에 패턴 몇 줄로 붙는다. 도커 이미지에서는 web 과 upcheck 가
컨테이너 두 개가 되고, web 이 `UPCHECK_API_URL` 로 upcheck 를 부른다 —
M5에서 주소를 `NEXT_PUBLIC_` 으로 박지 않은 이유가 거기서 드러난다.
