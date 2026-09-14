// 서버와 브라우저가 같은 결과를 내야 hydration 불일치가 안 난다.
// 그래서 시간대를 반드시 명시한다. 생략하면 서버(UTC 컨테이너)와
// 브라우저(KST)가 서로 다른 날짜를 찍는다.
//
// 시각(ISO 문자열) 포맷은 시간대를 인자로 받는다. 시간대 설정은 서버에서만
// 읽고(lib/api.ts), 서버 컴포넌트가 포맷한 문자열을 넘긴다.

const dayFmt = new Intl.DateTimeFormat("ko-KR", {
  month: "long",
  day: "numeric",
  weekday: "short",
  timeZone: "UTC", // Day.date 는 이미 시간대가 적용된 달력 날짜라 UTC 로 해석해야 안 밀린다
});



const numberFmt = new Intl.NumberFormat("ko-KR");

// "2026-09-14" 같은 달력 날짜를 Date 로. 시각이 붙은 문자열과 달리
// 이건 '그 날짜 자체'라서 UTC 자정으로 고정해 둔다.
function calendarDate(date: string) {
  return new Date(`${date}T00:00:00Z`);
}

export function formatDay(date: string) {
  return dayFmt.format(calendarDate(date));
}

// 축 눈금용 짧은 날짜. Intl 의 ko-KR 짧은 형식은 "6. 17." 처럼 꼬리 점이 붙어
// 좁은 축에서 지저분하다. 달력 날짜 문자열에서 바로 만든다.
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
  // 100 에 가까운 값은 소수점이 중요하다. 99.95 와 99.5 는 전혀 다르다.
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
