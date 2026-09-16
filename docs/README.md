# upcheck 학습 노트

이 폴더는 upcheck를 만들면서 필요한 **Go 개념과 문법**을 정리한 곳이다.
Go를 처음 접한다는 전제로 썼고, 설명마다 **이 레포의 실제 코드**를 예로 든다.

## 읽는 순서

| 문서 | 내용 | 언제 읽나 |
|------|------|-----------|
| [00-환경과-도구.md](00-환경과-도구.md) | 설치, `go` 명령어, 모듈, 폴더 구조 | 맨 처음 |
| [01-기초-문법.md](01-기초-문법.md) | 변수, 타입, 제로값, 함수, 제어문, 포인터 | 코드 읽기 전 |
| [02-복합-타입.md](02-복합-타입.md) | struct, slice, map, string/rune, 구조체 태그 | config 패키지 볼 때 |
| [03-메서드와-인터페이스.md](03-메서드와-인터페이스.md) | 메서드, 값/포인터 수신자, 인터페이스, `error` | checker 패키지 볼 때 |
| [04-에러-처리.md](04-에러-처리.md) | 에러가 값이라는 것, `%w`, `errors.Is/As`, `defer`/`panic` | 코드 전반 |
| [05-M0-순차-체크.md](05-M0-순차-체크.md) | M0에서 만든 것, 측정 결과, M1이 필요한 이유 | M0 끝나고 |
| [06-동시성-기초.md](06-동시성-기초.md) | goroutine, channel, select, `sync`, 누수 4가지 패턴 | scheduler 패키지 볼 때 |
| [07-context.md](07-context.md) | 취소 전파, 타임아웃, 취소되지 않는 것 | scheduler 패키지 볼 때 |
| [08-M1-동시성.md](08-M1-동시성.md) | 스케줄러 두 방식 비교와 선택 근거 (측정값) | M1 끝나고 |
| [09-데이터베이스.md](09-데이터베이스.md) | `database/sql`, SQLite 특성, 트랜잭션, goose, `embed` | store 패키지 볼 때 |
| [10-M2-저장과집계.md](10-M2-저장과집계.md) | 롤업 설계, 정확/근사의 경계, 측정값 | M2 끝나고 |
| [11-M3-알림.md](11-M3-알림.md) | 인터페이스 추상화, 상태 전이 3단 관문, 플래핑 방지 | M3 끝나고 |
| [12-M4-체크타입.md](12-M4-체크타입.md) | tcp / tls / dns, Prober 인터페이스, 경고와 실패의 구분 | M4 끝나고 |
| [13-HTTP-서버.md](13-HTTP-서버.md) | `http.Handler`, ServeMux 패턴, 미들웨어, JSON, 서버 타임아웃, Shutdown | api 패키지 볼 때 |
| [14-M5-API와-상태페이지.md](14-M5-API와-상태페이지.md) | API 설계, 상태 페이지 구조, 데모 데이터로 찾은 버그 | M5 끝나고 |
| [15-관측과-배포.md](15-관측과-배포.md) | Prometheus 지표, liveness/readiness, pprof, 멀티스테이지·distroless | metrics·admin·Dockerfile 볼 때 |
| [16-M6-운영준비.md](16-M6-운영준비.md) | 완료 기준 전체 점검, 프로젝트 전체 돌아보기 | M6 끝나고 |

모든 마일스톤(M0~M6)이 끝났습니다. [16번 문서](16-M6-운영준비.md) 마지막에
전체 완료 기준 점검과 되짚어 보는 정리가 있습니다.

## 참고 자료

- [A Tour of Go (한국어)](https://go.dev/tour/welcome/1) — 브라우저에서 바로 실행되는 공식 튜토리얼. 1~2시간이면 훑는다.
- [Effective Go](https://go.dev/doc/effective_go) — "Go답게 쓰는 법". 문법을 익힌 뒤 읽으면 좋다.
- [Go by Example](https://gobyexample.com/) — 주제별 짧은 예제 모음. 찾아보기용.
- [표준 라이브러리 문서](https://pkg.go.dev/std) — `net/http`, `context` 등을 직접 찾아볼 때.
