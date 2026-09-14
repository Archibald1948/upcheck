"use client";

import { useRouter } from "next/navigation";
import { useEffect } from "react";

/**
 * 주기적으로 서버 컴포넌트를 다시 렌더링한다.
 *
 * router.refresh() 는 페이지를 새로 불러오지 않는다. 서버에서 새 결과를 받아
 * 바뀐 부분만 갈아끼우고, 받는 동안에는 기존 화면을 그대로 둔다.
 * 스크롤 위치나 열어 둔 표 보기도 유지된다.
 *
 * 탭이 백그라운드에 있으면 멈춘다. 아무도 안 보는 탭이 계속 API 를 부를 이유가 없다.
 */
export function AutoRefresh({ seconds = 60 }: { seconds?: number }) {
  const router = useRouter();

  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | undefined;

    const start = () => {
      stop();
      timer = setInterval(() => router.refresh(), seconds * 1000);
    };
    const stop = () => timer && clearInterval(timer);

    const onVisibility = () => {
      if (document.hidden) {
        stop();
      } else {
        router.refresh(); // 돌아오면 바로 한 번
        start();
      }
    };

    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [router, seconds]);

  return null;
}
