"use client";

import { useEffect } from "react";

// 운영 환경에서는 서버 에러 원문(message)을 Next.js 가 가린다.
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
