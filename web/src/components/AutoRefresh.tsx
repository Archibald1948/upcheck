"use client";

import { useRouter } from "next/navigation";
import { useEffect } from "react";

// 탭이 백그라운드에 있으면 멈춘다.
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
