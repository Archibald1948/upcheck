# upcheck 상태 페이지

upcheck 가 수집한 결과를 보여주는 공개 상태 페이지. Next.js 16 (App Router).

## 실행

upcheck API 가 먼저 떠 있어야 한다. 저장소 루트에서:

```bash
go run ./cmd/seeddemo -db demo.db            # 데모 데이터 (처음 한 번)
go run ./cmd/upcheck -db demo.db -api-only   # API :8484
```

그다음 이 폴더에서:

```bash
npm install
npm run dev        # http://localhost:3000
```

## 환경변수

| 이름 | 기본값 | 뜻 |
|---|---|---|
| `UPCHECK_API_URL` | `http://localhost:8484` | upcheck API 주소. 서버에서만 읽는다 |
| `UPCHECK_TIMEZONE` | `Asia/Seoul` | 날짜 경계와 시각 표시 기준 시간대 |

둘 다 `NEXT_PUBLIC_` 이 아니다. 브라우저는 upcheck API 를 직접 부르지 않고,
빌드 시점이 아니라 실행 시점에 읽으므로 이미지 하나를 여러 환경에 배포할 수 있다.

## 구조

```
src/
├── lib/          API 클라이언트 · 타입 · 표시 유틸
├── components/   업타임 바 · 응답시간 차트 · 장애 목록
└── app/          / 와 /monitors/[id]
```

설계 근거는 [../docs/14-M5-API와-상태페이지.md](../docs/14-M5-API와-상태페이지.md).
