# 13 — HTTP 서버 (net/http · JSON)

`internal/api/server.go` 를 옆에 켜 두고 읽으면 좋다.
M0부터 `net/http` 를 **클라이언트**로 써 왔는데, 여기서는 **서버** 쪽이다.

## Handler — 인터페이스 하나가 전부다

```go
type Handler interface {
    ServeHTTP(ResponseWriter, *Request)
}
```

Go의 HTTP 서버는 이 메서드 하나짜리 인터페이스 위에 서 있다.
라우터도, 미들웨어도, 핸들러도 전부 `Handler` 다.
M3의 `Notifier`, M4의 `Prober` 와 똑같은 발상이다 (→ [03](03-메서드와-인터페이스.md)).

### HandlerFunc — 함수가 인터페이스를 구현한다

매번 구조체를 만들고 `ServeHTTP` 를 붙이긴 번거롭다. 그래서 표준 라이브러리에 이런 게 있다.

```go
type HandlerFunc func(ResponseWriter, *Request)

func (f HandlerFunc) ServeHTTP(w ResponseWriter, r *Request) {
    f(w, r)
}
```

**함수 타입에 메서드를 붙였다.** `func(w, r)` 모양의 함수를 `HandlerFunc(f)` 로
변환하기만 하면 `Handler` 가 된다. Go에서 메서드는 구조체 전용이 아니라는 것
(→ [03](03-메서드와-인터페이스.md))의 가장 유명한 예다.

```go
mux.HandleFunc("GET /api/status", s.handleStatus)
//             ↑ 내부에서 HandlerFunc(s.handleStatus) 로 감싼다
```

`s.handleStatus` 처럼 **메서드를 값으로** 넘길 수 있다(method value).
수신자 `s` 가 묶인 채로 넘어가서, 나중에 불려도 `s.store` 에 접근할 수 있다.

## ServeMux — Go 1.22 의 패턴 라우팅

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /api/status", s.handleStatus)
mux.HandleFunc("GET /api/monitors/{id}/history", s.handleHistory)
mux.HandleFunc("GET /api/incidents", s.handleIncidents)
mux.HandleFunc("GET /", notFound)
```

Go 1.22 전의 표준 `ServeMux` 는 경로 접두사만 볼 줄 알았다. 메서드 구분도,
`{id}` 같은 경로 변수도 없어서 `chi` 나 `gorilla/mux` 를 들이는 게 당연했다.
지금은 표준만으로 된다. 스펙 3절의 "stdlib 라우팅으로 충분"이 이 뜻이다.

### 경로 변수

```go
id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
```

`r.PathValue("id")` 는 **문자열**을 준다. 숫자인지는 우리가 확인해야 한다.
`/api/monitors/abc/history` 도 패턴에는 맞는다.

### 메서드 패턴과 405

`"GET /api/status"` 로 등록하면
- `GET`, `HEAD` → 처리
- `POST` → **405 Method Not Allowed**, 허용 메서드를 알려주는 `Allow` 헤더까지 자동

### 함정: 메서드 없는 catch-all 이 405 를 삼킨다

처음에는 없는 경로에 JSON 404 를 주려고 이렇게 썼다.

```go
mux.HandleFunc("/", notFound)   // ← 메서드 없음
```

그랬더니 `POST /api/status` 가 405 가 아니라 **404** 가 됐다.

ServeMux 는 요청에 맞는 패턴을 찾는다. `"GET /api/status"` 는 경로는 맞지만
메서드가 틀려서 탈락하고, `"/"` 는 **모든 메서드·모든 경로**에 맞으니 거기로 간다.
"경로는 맞는데 메서드만 틀렸다"고 판단할 기회가 사라진 것이다.

```go
mux.HandleFunc("GET /", notFound)   // ← 메서드를 붙인다
```

이제 `POST /api/status` 에 맞는 패턴이 하나도 없어서 ServeMux 가 405 를 준다.
`TestMethodNotAllowed` 가 이걸 고정한다.

### 우선순위

패턴이 여러 개 맞으면 **더 구체적인 쪽**이 이긴다. `"GET /api/status"` 가
`"GET /"` 보다 구체적이라 등록 순서와 무관하게 먼저 잡힌다.
둘 중 어느 쪽이 더 구체적인지 가릴 수 없게 겹치면 등록할 때 panic 한다.

## ResponseWriter — 순서가 있다

```go
func writeJSON(w http.ResponseWriter, status int, v any) {
    w.Header().Set("Content-Type", "application/json; charset=utf-8")   // ① 헤더
    w.WriteHeader(status)                                                // ② 상태 코드
    json.NewEncoder(w).Encode(v)                                         // ③ 본문
}
```

HTTP 응답은 헤더를 먼저 보내고 본문을 보낸다. 그래서 순서가 강제된다.

- `WriteHeader` 이후의 `Header().Set` 은 **조용히 무시된다.**
- `WriteHeader` 없이 `Write` 하면 그 순간 **200** 으로 확정된다.
- 본문을 쓰다가 에러가 나도 **상태 코드를 바꿀 수 없다.** 이미 나갔다.

마지막 항목 때문에 `Encode` 의 에러를 `_` 로 버린다. 받아도 할 수 있는 게 없다.

## 미들웨어 — Handler 를 받아 Handler 를 돌려준다

```go
func (s *Server) logRequests(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        next.ServeHTTP(w, r)           // 안쪽 핸들러 실행
        s.log.Debug("API 요청", "elapsed", time.Since(start))
    })
}
```

특별한 프레임워크가 필요 없다. `func(http.Handler) http.Handler` 모양의 함수면 된다.
감싸는 순서대로 양파처럼 겹친다.

```go
var h http.Handler = mux
h = s.logRequests(h)
h = s.recoverPanics(h)
// 요청은 recover → log → mux 순으로 지나간다
```

### 상태 코드를 엿보기 — 임베딩으로 감싸기

로그에 상태 코드를 남기고 싶은데, `ResponseWriter` 에는 "무슨 코드를 썼냐"고
물어볼 메서드가 없다. 감싸서 가로챈다.

```go
type statusRecorder struct {
    http.ResponseWriter   // 임베딩 — Header(), Write() 가 그대로 위임된다
    status int
}

func (r *statusRecorder) WriteHeader(code int) {
    r.status = code
    r.ResponseWriter.WriteHeader(code)   // 원래 것도 불러준다
}
```

인터페이스를 **임베딩**하면 모든 메서드가 딸려 온다 (→ [03](03-메서드와-인터페이스.md)의 임베딩).
바꾸고 싶은 `WriteHeader` 하나만 새로 정의하면 된다.

### panic 복구 — ErrAbortHandler 는 다시 던진다

```go
defer func() {
    if v := recover(); v != nil {
        if v == http.ErrAbortHandler {
            panic(v)
        }
        writeError(w, http.StatusInternalServerError, "내부 오류")
    }
}()
```

`net/http` 는 사실 요청 goroutine 의 panic 을 스스로 잡는다(서버가 죽지 않는다).
그래도 미들웨어를 두는 건 클라이언트가 **끊긴 연결 대신 500 JSON** 을 받고,
스택을 **우리 로거**로 남기기 위해서다.

`http.ErrAbortHandler` 는 `net/http` 가 "조용히 연결을 끊어라"는 신호로 쓰는
특별한 panic 값이다. 이걸 삼키면 원래 동작이 깨지니 다시 던진다.

## encoding/json

### 구조체 태그로 모양을 정한다

```go
type MonitorStatus struct {
    ID        int64      `json:"id"`
    Target    string     `json:"target,omitempty"`   // 비면 필드째 빠진다
    LastCheck *time.Time `json:"last_checked_at"`    // nil 이면 null
}
```

YAML 때와 같다 (→ [02](02-복합-타입.md)). 소문자 필드는 리플렉션이 못 봐서 **조용히 빠진다.**

### null 과 0 을 구분하려면 포인터

```go
type UptimeWindows struct {
    H24 *float64 `json:"24h"`
}
```

`float64` 로 두면 데이터가 없을 때 `0` 이 나간다. 업타임 `0%` 는
"완전히 죽어 있었다"는 뜻이라 **거짓말**이 된다. 포인터로 두면 `null` 이 나간다.
M0의 `Enabled *bool`(→ [01](01-기초-문법.md))과 같은 이유다.

포인터 필드를 채울 때는 지역 변수의 주소를 쓴다.

```go
v := round2(u.Percent())
return &v       // Go는 이게 안전하다 — 탈출 분석이 v 를 힙으로 옮겨준다
```

C였다면 함수가 끝나면 사라지는 지역 변수의 주소를 돌려주는 버그다.
Go는 컴파일러가 "이 변수는 함수 밖으로 새어 나간다"를 감지해 알아서 힙에 둔다.

### time.Duration 을 그대로 내보내지 않는다

`Duration` 은 `int64` 라서 JSON 에 **나노초 정수**로 찍힌다.
`{"p95": 322000000}` 를 받은 JavaScript 는 그게 나노초인지 알 길이 없다.

```go
P95MS *int64 `json:"p95_ms"`   // 단위를 이름에 박는다
```

`time.Time` 은 반대로 알아서 RFC 3339 문자열(`"2026-09-14T06:00:00Z"`)로 나간다.
그래서 항상 `.UTC()` 로 바꿔서 넣는다. 섞이면 받는 쪽이 헷갈린다.

### 저장소 타입을 그대로 내보내지 않는다

`store.MonitorStatus` 를 바로 `Encode` 할 수도 있었다. 하지 않은 이유:

- 저장소 타입은 내부 사정으로 바뀐다. 필드 이름 하나 바꿀 때마다 프론트엔드가 깨진다.
- 공개하면 안 되는 필드(target 주소)가 **실수로 새어 나간다.**

API 응답 타입(`internal/api/types.go`)을 따로 두고 변환 함수를 거친다.
타이핑은 늘지만 경계가 생긴다.

## http.Server — 기본값을 믿으면 안 된다

```go
srv := &http.Server{
    Handler:           s.Handler(),
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       10 * time.Second,
    WriteTimeout:      30 * time.Second,
    IdleTimeout:       120 * time.Second,
}
```

`http.ListenAndServe(addr, h)` 한 줄로도 서버는 뜬다. 하지만 그 서버는
**타임아웃이 전부 0(무제한)** 이다.

헤더를 1초에 한 바이트씩 흘리는 클라이언트 수백 개면 연결을 전부 붙잡을 수 있다.
이걸 **Slowloris** 공격이라고 한다. `ReadHeaderTimeout` 하나만 있어도 막힌다.
정적 분석 도구 gosec 이 `ReadHeaderTimeout` 없는 서버를 경고(G112)하는 이유다.

M0의 `cmd/slowserver` 에도 `ReadHeaderTimeout` 을 넣어 두었다.

## 요청 ctx 와 서버 수명

```go
BaseContext: func(net.Listener) context.Context { return ctx },
```

요청마다 `r.Context()` 가 생기는데, 그 **부모**를 서버 수명 ctx 로 정한다.
Ctrl+C 가 오면 진행 중인 요청의 DB 질의까지 함께 취소된다 (→ [07](07-context.md)).

`r.Context()` 는 클라이언트가 연결을 끊어도 취소된다. 그래서 핸들러에서
`context.Canceled` 에러는 "우리 잘못이 아니다"로 보고 로그를 남기지 않는다.

## Listen 과 Serve 를 나누기

```go
func Listen(addr string) (net.Listener, error)
func (s *Server) Serve(ctx context.Context, ln net.Listener) error
```

`ListenAndServe` 하나로 합쳐 두고 goroutine 에서 부르면 이렇게 된다.

```go
go func() { apiDone <- srv.ListenAndServe(ctx, addr) }()
// 포트가 이미 쓰이고 있으면? 에러는 apiDone 에 들어가고
// 아무도 종료 때까지 그걸 읽지 않는다.
```

**체크는 잘 도는데 상태 페이지만 안 뜨는** 상태가 된다. 로그를 뒤지기 전까지 모른다.

포트를 잡는 부분(`Listen`)만 먼저 동기로 부르면, 실패는 바로 에러로 돌아온다.

```
$ upcheck -duration 10s     # 8484 를 다른 프로세스가 쓰는 중
오류: API 서버 listen 실패 (:8484): listen tcp :8484: bind: address already in use
# 종료 코드 1 · 소요 0초 — 10초짜리 체크를 시작하기도 전에 실패한다
```

## Shutdown — 우아한 종료

```go
select {
case err := <-serveErr:
    return err
case <-ctx.Done():
}

shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
srv.Shutdown(shutdownCtx)
```

`Shutdown` 은
1. 리스너를 닫아 **새 연결을 막고**
2. 유휴 연결을 닫고
3. **처리 중인 요청이 끝나기를** 기다린다

시한이 지나면 기다리기를 포기하고 에러를 돌려준다.
`context.Background()` 에서 새로 시작하는 건 M2·M3 와 같은 이유다 —
이미 취소된 ctx 의 자식은 태어나자마자 취소되어 있다.

`Serve` 는 `Shutdown` 이 불리면 `http.ErrServerClosed` 를 돌려준다.
이건 **정상 종료 신호**라 에러로 취급하지 않는다.

```go
if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
    serveErr <- err
}
```

### 종료 순서가 틀리면 생기는 일

`cmd/upcheck/main.go` 에서 API 서버가 내려가기를 **기다린 뒤에** 반환한다.
기다리지 않으면 `run()` 의 `defer st.Close()` 가 먼저 돌아서, 응답하던 요청이
**닫힌 DB** 를 만난다.

## 실패할 수 있는 준비는 goroutine 앞에서

M5에서 main 을 고치다 이런 코드를 만들었다.

```go
go func() { for ev := range d.events { ... } }()   // ① goroutine 시작

if err := engine.Restore(ctx); err != nil {
    return err                                       // ② 여기서 반환하면?
}
```

②에서 반환하면 아무도 `d.events` 를 닫지 않는다. ①의 goroutine 은
`range` 에서 **영원히** 기다린다. [06](06-동시성-기초.md) 누수 패턴 2번 그대로다.

고친 규칙: **에러로 일찍 반환할 수 있는 일(장애 복원, 포트 잡기)을 전부 끝낸 뒤에
goroutine 을 띄운다.** 그 뒤로는 일찍 반환하지 않는다.

## 테스트 — httptest

### 핸들러만 시험할 때: NewRecorder

```go
rec := httptest.NewRecorder()
h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

rec.Code                         // 상태 코드
rec.Header().Get("Allow")        // 헤더
rec.Body.Bytes()                 // 본문
```

진짜 네트워크를 안 탄다. `Handler` 가 인터페이스라서 가능한 일이다 —
`ResponseWriter` 자리에 기록만 하는 가짜를 넣는다.

### 서버째 시험할 때: NewServer

M0부터 checker 테스트에서 써 온 `httptest.NewServer` 는 **진짜 포트**를 연다.
클라이언트 코드(타임아웃, 재시도)를 시험할 때 쓴다.

### 제네릭 헬퍼

```go
func decode[T any](t *testing.T, body []byte) T {
    var v T
    json.Unmarshal(body, &v)
    return v
}

resp := decode[StatusResponse](t, body)
```

`[T any]` 가 **타입 매개변수**다(Go 1.18+). 응답 타입마다 똑같은 해석 코드를
복사하지 않고 하나로 쓴다. 호출할 때 `decode[StatusResponse]` 처럼 타입을 넘긴다.

Go 제네릭은 이런 **작고 반복적인 유틸**에 쓸 때 가장 빛난다.
구조를 추상화하는 데는 여전히 인터페이스가 먼저다.

### 정보 노출 테스트

```go
for _, secret := range []string{"10.0.3.17", "5432", "internal-api.corp", "connection refused"} {
    if strings.Contains(string(body), secret) {
        t.Errorf("%s 응답에 내부 정보 %q 가 새어 나갔다", path, secret)
    }
}
```

"숨기는 코드가 있다"가 아니라 **"응답 문자열 어디에도 없다"** 를 확인한다.
나중에 누가 필드를 추가해도 이 테스트가 잡는다.

## 다음

→ [14-M5-API와-상태페이지.md](14-M5-API와-상태페이지.md)
