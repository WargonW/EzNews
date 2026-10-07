/**
 * 在线/离线状态。
 *
 * 用于展示离线态。**只用于提示，绝不阻断浏览**：
 * 离线时已缓存的内容照常可读（PRD R1「游客永不被打断」）。
 */

import { useEffect, useState } from 'react';

/** 返回当前是否在线（navigator.onLine + online/offline 事件）。 */
export function useOnlineStatus(): boolean {
  const [online, setOnline] = useState(() =>
    typeof navigator === 'undefined' ? true : navigator.onLine,
  );

  useEffect(() => {
    const goOnline = (): void => setOnline(true);
    const goOffline = (): void => setOnline(false);
    window.addEventListener('online', goOnline);
    window.addEventListener('offline', goOffline);
    return () => {
      window.removeEventListener('online', goOnline);
      window.removeEventListener('offline', goOffline);
    };
  }, []);

  return online;
}