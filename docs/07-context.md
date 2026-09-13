# 07 — context

`context.Context` 는 **취소 신호와 시한을 goroutine 트리 전체에 퍼뜨리는** 장치다.
동시성 프로그램에서 "이제 그만"을 전달하는 Go의 표준 방법이다.

## 왜 필요한가

Ctrl+C 를 눌렀다고 하자. 그 순간 upcheck 안에서는

- 스케줄러 goroutine 이 tick 을 기다리고 있고
- 워커 8개가 HTTP 응답을 기다리고 있고
- 그 밑에서 `net/http` 가 소켓을 읽고 있다

이 전부에게 "멈춰"를 전해야 한다. 전역 플래그를 두고 다들 들여다보게 할 수도 있지만,
**남의 라이브러리 안쪽까지는 전할 수 없다.** context 는 그걸 위한 공통 규약이다.
`net/http`, `database/sql` 전부 context 를 받는다.

## 인터페이스

```go
type Context interface {
    Done() <-chan struct{}      // 취소되면 닫히는 채널
    Err() error                 // 왜 취소됐는지 (아직 살아 있으면 nil)
    Deadline() (time.Time, bool)
    Value(key any) any
}
```

핵심은 `Done()` 이다. **취소되면 채널이 닫힌다.**
닫힌 채널은 받기를 시도하는 모두에게 즉시 제로값을 주므로,
기다리던 goroutine이 **전부 동시에** 깨어난다. 이게 취소 전파의 실체다.

```go
select {
case <-ctx.Done():
    return
case <-ticker.C:
}
```

## 만드는 법

```go
context.Background()                        // 뿌리. main 에서 시작
context.TODO()                              // "나중에 정할 것" 표시용

ctx, cancel := context.WithCancel(parent)          // 손으로 취소
ctx, cancel := context.WithTimeout(parent, 5*time.Second)  // 상대 시간
ctx, cancel := context.WithDeadline(parent, t)     // 절대 시각
```

`cancel` 을 반환하는 것들은 **반드시 불러야 한다.** 안 부르면 내부 타이머와
goroutine 이 남는다 (누수 패턴 3번). 얻는 즉시 `defer cancel()`.

```go
ctx, cancel := context.WithTimeout(ctx, m.Timeout())
defer cancel()
```

> `go vet` 의 `lostcancel` 검사가 빠뜨린 cancel 을 잡아준다.

## 트리 구조 — 취소는 아래로만 흐른다

context 는 부모-자식 트리다. **부모가 취소되면 모든 자손이 함께 취소된다.**
반대는 없다. 자식을 취소해도 부모는 멀쩡하다.

upcheck 의 트리:

```
context.Background()
  └─ signal.NotifyContext      ← SIGINT/SIGTERM 이면 취소   (main.go)
      └─ WithTimeout(-duration) ← 지정 시간이 지나면 취소    (main.go)
          └─ 스케줄러 / 워커들이 공유
              └─ WithTimeout(timeout_ms) ← 체크 하나의 시한 (checker.go)
                  └─ http.Request
```

가장 아래 HTTP 요청까지 한 줄로 이어져 있다.
그래서 **Ctrl+C 한 번에 진행 중인 소켓 읽기까지 즉시 끊긴다.**

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()

if *duration > 0 {
    var cancel context.CancelFunc
    ctx, cancel = context.WithTimeout(ctx, *duration)
    defer cancel()
}
```

`-duration` 이 자식이라 **Ctrl+C(부모)도 여전히 먹는다.** 둘 중 먼저 오는 쪽이 이긴다.

## 취소된 이유 구분하기

```go
ctx.Err()
// nil                        아직 살아 있음
// context.Canceled           누가 cancel() 을 불렀다
// context.DeadlineExceeded   시한이 지났다
```

이걸로 "타임아웃이라 실패"와 "종료 중이라 취소됨"을 구분한다.
전자는 진짜 장애고 후자는 우리가 끈 것이다. DB에 장애로 기록하면 안 된다.

```go
func classifyErr(ctx context.Context, err error) error {
    switch ctx.Err() {
    case context.DeadlineExceeded:
        return fmt.Errorf("타임아웃")
    case context.Canceled:
        return fmt.Errorf("취소됨(종료 중)")
    }
    return err
}
```
→ `internal/checker/checker.go`

에러를 감싸서 받았다면 `errors.Is` 를 쓴다.

```go
if errors.Is(err, context.DeadlineExceeded) { ... }
```

## HTTP 에 붙이기

```go
req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Target, nil)
```

`http.NewRequest`(context 없는 버전)가 아니라 이걸 쓴다.
ctx 가 취소되면 진행 중인 요청이 **즉시** 끊긴다.

### `http.Client.Timeout` 을 쓰지 않은 이유

```go
&http.Client{
    Transport: transport,
    // Timeout 을 일부러 비워 둔다
}
```

| | `Client.Timeout` | context 타임아웃 |
|---|---|---|
| 범위 | 요청 하나 전체 | 요청 하나 전체 |
| 상위 취소에 반응 | **안 한다** | 한다 |
| 요청마다 다르게 | 클라이언트를 여러 개 만들어야 | 그냥 다르게 준다 |

`Client.Timeout` 만 쓰면 Ctrl+C 를 눌러도 **타임아웃이 끝날 때까지 기다려야 한다.**
모니터마다 `timeout_ms` 가 다른데 클라이언트는 하나를 공유해야 하는
(커넥션 풀 때문에) 사정도 있다. 그래서 context 로 건다.

스펙 6절 완료 기준 "Ctrl+C 시 5초 내 종료"가 여기서 결정된다.
`internal/scheduler/scheduler_test.go` 의 `TestShutdownIsPrompt` 가 이걸 검증한다.
3초짜리 느린 서버를 상대로 체크가 진행 중일 때 취소하면 **1ms 안에** 끝난다.

## 규칙

### 1. 첫 번째 인자로 받는다

```go
func (c *Checker) Check(ctx context.Context, m config.Monitor) Result
func (p *Pool) Run(ctx context.Context, monitors []config.Monitor) <-chan checker.Result
```

이름은 `ctx`. 예외 없이 첫 자리다.

### 2. 구조체에 저장하지 않는다

```go
type Checker struct {
    ctx context.Context   // X 하지 말 것
}
```

context 는 **요청 하나의 수명**을 나타내지 객체의 속성이 아니다.
같은 Checker 를 여러 요청이 쓰는데 ctx 를 박아두면 수명이 뒤엉킨다.

(드문 예외: 오래 도는 워커 구조체가 자기 수명 ctx 를 들고 있는 경우.
그래도 메서드 인자로 받는 쪽을 먼저 고려한다)

### 3. nil 을 넘기지 않는다

정할 게 없으면 `context.TODO()`.

### 4. Value 는 최소한으로

```go
ctx = context.WithValue(ctx, requestIDKey{}, id)
```

**함수 인자 대신 쓰라고 있는 게 아니다.** 요청 ID, 추적 정보처럼
"코드 전 구간을 따라다니지만 로직에는 안 쓰이는 것"에만 쓴다.
타입 검사가 안 되고(`any`), 어디서 뭐가 들어오는지 추적이 어렵다.

키는 반드시 **직접 정의한 타입**으로 한다. `string` 을 쓰면 다른 패키지와 충돌한다.

```go
type requestIDKey struct{}   // 이 패키지만 아는 타입
```

## select 를 빠뜨리면 생기는 일

context 를 받아만 놓고 **감시하지 않으면** 아무 효과가 없다.

```go
// X 나쁜 예 — ctx 를 받지만 쓰지 않는다
func worker(ctx context.Context, jobs <-chan Job) {
    for job := range jobs {    // jobs 가 닫힐 때까지 영원히
        process(job)
    }
}

// O 좋은 예
func worker(ctx context.Context, jobs <-chan Job) {
    for {
        select {
        case <-ctx.Done():
            return
        case job, ok := <-jobs:
            if !ok {
                return
            }
            process(job)
        }
    }
}
```

**보내는 쪽도 마찬가지다.** 이걸 빠뜨리는 게 제일 흔한 누수다.

```go
select {
case results <- res:
case <-ctx.Done():
    return
}
```

## 취소되지 않는 것도 있다

context 는 만능이 아니다. **협조하는 코드만** 멈춘다.

실제로 이 프로젝트에서 확인한 사례다. ticker 스케줄러로 모니터 50개를
동시에 돌린 뒤 취소하고 남은 goroutine 을 떠 봤다.

```
goroutine profile: total 41
35 @ ...
#	net.(*Resolver).lookupIPAddr+0x2f7	net/lookup.go:343
#	net.(*Resolver).internetAddrList+0x4bb	net/ipsock.go:289
#	net/http.(*Transport).dialConn+0x667	net/http/transport.go:1920
```

**35개가 DNS 조회에 묶여 있었다.** `lookupIPAddr` 은 context 가 취소되면
호출자에게는 즉시 돌아오지만, 실제 DNS 질의를 하는 goroutine 은
질의가 끝날 때까지 남는다. OS 리졸버 호출을 중간에 끊을 방법이 없기 때문이다.

여기서 두 가지를 배운다.

1. **취소했다고 자원이 즉시 반납되는 건 아니다.** 종료 직후 goroutine 수를
   재는 테스트는 이런 잔여물을 감안해야 한다.
2. **동시성 상한이 종료 시점에도 영향을 준다.** 동시에 50개를 띄웠으면
   취소 못 하는 잔여물도 50개분 남는다. 워커풀로 8개로 묶으면 그만큼 적다.

같은 이유로 종료할 때 유휴 커넥션을 정리한다.

```go
func (c *Checker) Close() {
    c.client.CloseIdleConnections()
}
```

안 불러도 `IdleConnTimeout`(90초) 이 지나면 닫히지만, 그때까지
커넥션과 거기 딸린 goroutine 이 남는다.

## 다음

→ [08-M1-동시성.md](08-M1-동시성.md)
