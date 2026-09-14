"use client";

import { useEffect } from "react";

// error.tsx 는 클라이언트 컴포넌트여야 한다. 서버에서 난 에러를 받아
// 브라우저에서 "다시 시도"를 할 수 있어야 하기 때문이다.
//
// 서버 에러의 원문(message)은 운영 환경에서 브라우저로 넘어오지 않는다.
// Next.js 가 일부러 가린다 — 내부 주소나 스택이 새지 않게.
export default function ErrorPage({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  useEffect(() => {
    console.error(error);
  }, [error]);

  return (
    <div className="card">
      <h1 style={{ fontSize: 20, margin: "0 0 6px" }}>상태 정보를 불러오지 못했습니다</h1>
      <p className="secondary" style={{ margin: "0 0 14px" }}>
        상태 수집 서버(upcheck)에 연결할 수 없습니다. 잠시 후 다시 시도해 주세요.
      </p>
      <button
        type="button"
        onClick={reset}
        style={{
          font: "inherit",
          padding: "6px 14px",
          borderRadius: 8,
          border: "1px solid var(--border)",
          background: "var(--surface)",
          color: "var(--text-primary)",
          cursor: "pointer",
        }}
      >
        다시 시도
      </button>
    </div>
  );
}
