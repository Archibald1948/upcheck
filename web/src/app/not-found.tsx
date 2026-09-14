import Link from "next/link";

export default function NotFound() {
  return (
    <div className="card">
      <h1 style={{ fontSize: 20, margin: "0 0 6px" }}>찾을 수 없는 페이지입니다</h1>
      <p className="secondary" style={{ margin: "0 0 14px" }}>
        없는 모니터이거나 감시를 중단한 모니터입니다.
      </p>
      <Link href="/">전체 상태로 돌아가기</Link>
    </div>
  );
}
