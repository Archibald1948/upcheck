# 상태 페이지 이미지 (멀티스테이지)

# ─────────────── 1단계: 의존성 ───────────────
FROM node:22-alpine AS deps
WORKDIR /app
# package.json 과 lock 파일만 먼저 복사한다. 소스가 바뀌어도
# 의존성이 그대로면 이 레이어는 캐시에서 재사용된다.
COPY package.json package-lock.json ./
# npm ci 는 lock 파일 그대로 설치한다. npm install 과 달리 lock 을 고치지 않아서
# 빌드가 재현 가능하다.
RUN npm ci

# ─────────────── 2단계: 빌드 ───────────────
FROM node:22-alpine AS build
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN npm run build

# ─────────────── 3단계: 실행 ───────────────
FROM node:22-alpine AS run
WORKDIR /app
ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1 \
    PORT=3000 \
    HOSTNAME=0.0.0.0

# standalone 출력에는 실행에 필요한 node_modules 만 추려져 있다.
# 전체 node_modules(수백 MB)를 옮기지 않아도 된다.
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static

# node 이미지에 이미 있는 비루트 사용자
USER node

EXPOSE 3000

# standalone 빌드가 만들어 준 서버. next CLI 가 필요 없다.
CMD ["node", "server.js"]
