# upcheck 데몬 이미지 (멀티스테이지)

# ─────────────── 1단계: 빌드 ───────────────
FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

#   -trimpath : 빌드 머신의 파일 경로를 바이너리에서 지운다
#   -s -w     : 디버그 심볼과 DWARF 를 빼서 크기를 줄인다
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/upcheck ./cmd/upcheck

# distroless 에는 mkdir/chown 이 없어서 여기서 만들어 둔다.
RUN mkdir -p /data && chown 65532:65532 /data

# ─────────────── 2단계: 실행 ───────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/upcheck /usr/local/bin/upcheck
COPY --from=build --chown=65532:65532 /data /data
COPY configs/monitors.yaml /etc/upcheck/monitors.yaml

# nonroot 는 distroless 가 만들어 둔 uid 65532 사용자다.
USER nonroot:nonroot

EXPOSE 8484 8485
VOLUME ["/data"]

# 셸이 없으므로 exec 형식만 쓸 수 있다.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/upcheck", "-healthcheck", "-admin", "127.0.0.1:8485"]

ENTRYPOINT ["/usr/local/bin/upcheck"]
CMD ["-config", "/etc/upcheck/monitors.yaml", \
     "-db", "/data/upcheck.db", \
     "-http", ":8484", \
     "-admin", "0.0.0.0:8485", \
     "-quiet"]
