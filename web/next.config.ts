import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // 도커 이미지를 위해 실행에 필요한 파일만 추린 서버를 만든다.
  // .next/standalone 에 server.js 와 최소 node_modules 가 들어간다.
  output: "standalone",

  // 응답 헤더에서 x-powered-by 를 뺀다. 쓰는 프레임워크를 굳이 알릴 이유가 없다.
  poweredByHeader: false,
};

export default nextConfig;
