const dayFmt = new Intl.DateTimeFormat("ko-KR", {
  month: "long",
  day: "numeric",
  weekday: "short",
  timeZone: "UTC", // Day.date 는 이미 시간대가 적용된 달력 날짜라 UTC 로 해석해야 안 밀린다
});

const numberFmt = new Intl.NumberFormat("ko-KR");

function calendarDate(date: string) {
  return new Date(`${date}T00:00:00Z`);
}

export function formatDay(date: string) {
  return dayFmt.format(calendarDate(date));
}

export function formatShortDay(date: string) {
  const [, m, d] = date.split("-");
  return `${Number(m)}/${Number(d)}`;
}

export function formatDateTime(iso: string, timeZone: string) {
  return new Intl.DateTimeFormat("ko-KR", {
    month: "long",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZone,
  }).format(new Date(iso));
}

export function formatNumber(n: number) {
  return numberFmt.format(n);
}

export function formatUptime(u: number | null) {
  if (u === null) return "—";
  return `${u === 100 ? "100" : u.toFixed(2)}%`;
}

export function formatMs(ms: number | null) {
  if (ms === null) return "—";
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)}초`;
  return `${numberFmt.format(ms)}ms`;
}

export function formatDuration(sec: number) {
  if (sec < 60) return `${sec}초`;
  const m = Math.round(sec / 60);
  if (m < 60) return `${m}분`;
  const h = Math.floor(m / 60);
  const rest = m % 60;
  if (h < 24) return rest ? `${h}시간 ${rest}분` : `${h}시간`;
  const d = Math.floor(h / 24);
  return `${d}일 ${h % 24}시간`;
}
