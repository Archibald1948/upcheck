import type { Level } from "@/lib/status";

// 상태 색은 절대 혼자 뜻을 전하지 않는다. 모양이 다른 아이콘을 함께 쓴다.
// 색각 이상이 있거나 흑백으로 인쇄해도 정상/장애가 구분돼야 한다.
export function StatusIcon({ level, size = 16 }: { level: Level; size?: number }) {
  const color = `var(--status-${level})`;
  const common = {
    width: size,
    height: size,
    viewBox: "0 0 16 16",
    "aria-hidden": true,
    style: { flex: "none" },
  } as const;

  switch (level) {
    case "good":
      return (
        <svg {...common}>
          <circle cx="8" cy="8" r="8" fill={color} />
          <path d="M4.5 8.2l2.3 2.3 4.7-4.9" fill="none" stroke="#fff" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
    case "warning":
    case "serious":
      return (
        <svg {...common}>
          <path d="M8 1.2l7 12.6H1z" fill={color} strokeLinejoin="round" />
          <path d="M8 6v3.6" stroke="#0b0b0b" strokeWidth="1.6" strokeLinecap="round" />
          <circle cx="8" cy="11.7" r="0.95" fill="#0b0b0b" />
        </svg>
      );
    case "critical":
      return (
        <svg {...common}>
          <circle cx="8" cy="8" r="8" fill={color} />
          <path d="M5.3 5.3l5.4 5.4M10.7 5.3l-5.4 5.4" stroke="#fff" strokeWidth="1.8" strokeLinecap="round" />
        </svg>
      );
    default:
      return (
        <svg {...common}>
          <circle cx="8" cy="8" r="7" fill="none" stroke="var(--text-muted)" strokeWidth="1.5" />
          <path d="M5 8h6" stroke="var(--text-muted)" strokeWidth="1.6" strokeLinecap="round" />
        </svg>
      );
  }
}
