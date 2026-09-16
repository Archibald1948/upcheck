# upcheck 데몬 이미지 (멀티스테이지)
#
# 1단계에서 Go 툴체인으로 빌드하고, 2단계에는 바이너리만 옮긴다.
# 최종 이미지에 컴파일러·소스·패키지 매니저가 남지 않는다.

# ─────────────── 1단계: 빌드 ───────────────
FROM golang:1.27-alpine AS build

WORKDIR /src

# go.mod/go.sum 을 먼저 복사해 의존성만 받는다.
# 소스가 바뀌어도 이 레이어는 캐시에 남아서, 매번 다시 받지 않는다.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 이 핵심이다. C 라이브러리에 링크하지 않은 정적 바이너리가 나와서
# libc 조차 없는 distroless/static 이미지에서 그대로 돈다.
# (M2에서 순수 Go SQLite 드라이버를 고른 이유가 여기서 값을 한다)
#
#   -trimpath : 빌드 머신의 파일 경로를 바이너리에서 지운다
#   -s -w     : 디버그 심볼과 DWARF 를 빼서 크기를 줄인다
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/upcheck ./cmd/upcheck

# 데이터 디렉터리를 미리 만들어 소유자를 맞춰 둔다.
# distroless 에는 mkdir 도 chown 도 없어서 최종 단계에서는 못 만든다.
# 도커가 빈 볼륨을 처음 붙일 때 이미지의 디렉터리 소유권을 그대로 가져간다.
RUN mkdir -p /data && chown 65532:65532 /data

# ─────────────── 2단계: 실행 ───────────────
# distroless/static: 셸도, 패키지 매니저도, libc 도 없다.
# 들어 있는 건 CA 인증서와 최소한의 /etc 뿐이다.
# 공격 표면이 작고, 이미지 안에서 할 수 있는 일이 거의 없다.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/upcheck /usr/local/bin/upcheck
COPY --from=build --chown=65532:65532 /data /data
COPY configs/monitors.yaml /etc/upcheck/monitors.yaml

# nonroot 는 distroless 가 만들어 둔 uid 65532 사용자다.
USER nonroot:nonroot

EXPOSE 8484 8485
VOLUME ["/data"]

# 셸이 없으므로 exec 형식으로 쓴다. 셸 형식(CMD command)은 /bin/sh 를 찾다가 실패한다.
# exec 형식이면 PID 1 이 upcheck 자신이 되어 SIGTERM 을 직접 받는다.
# (셸을 거치면 셸이 PID 1 이 되어 신호를 전달하지 않아, 종료가 10초 후 강제 종료가 된다)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/upcheck", "-healthcheck", "-admin", "127.0.0.1:8485"]

ENTRYPOINT ["/usr/local/bin/upcheck"]
CMD ["-config", "/etc/upcheck/monitors.yaml", \
     "-db", "/data/upcheck.db", \
     "-http", ":8484", \
     "-admin", "0.0.0.0:8485", \
     "-quiet"]
