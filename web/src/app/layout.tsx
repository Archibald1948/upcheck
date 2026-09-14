import type { Metadata } from "next";
import Link from "next/link";
import "./globals.css";

export const metadata: Metadata = {
  title: "서비스 상태",
  description: "upcheck 가 감시하는 서비스들의 실시간 상태와 90일 가용성",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="ko">
      <body>
        <div className="shell">
          <header className="site-header">
            <Link href="/" className="site-title">
              서비스 상태
            </Link>
            <span className="site-meta">upcheck</span>
          </header>
          <main>{children}</main>
        </div>
      </body>
    </html>
  );
}
