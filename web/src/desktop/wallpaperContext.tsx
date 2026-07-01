import { useCallback, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import {
  BUNDLED_WALLPAPERS,
  STORAGE_KEY,
  WallpaperContext,
  loadPrefs,
  type WallpaperPrefs,
} from './wallpaper';

const ROTATE_INTERVAL = 30 * 60 * 1000;

export function WallpaperProvider({ children }: { children: ReactNode }) {
  const [prefs, setPrefs] = useState<WallpaperPrefs>(loadPrefs);

  const update = useCallback((patch: Partial<WallpaperPrefs>) => {
    setPrefs((prev) => {
      const next = { ...prev, ...patch };
      try {
        localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
      } catch {
        // ignore storage write failures (private mode, quota, etc.)
      }
      return next;
    });
  }, []);

  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  useEffect(() => {
    if (!prefs.autoRotate) {
      if (timerRef.current) {
        clearInterval(timerRef.current);
        timerRef.current = null;
      }
      return;
    }
    const rotate = () => {
      const idx = Math.floor(Math.random() * BUNDLED_WALLPAPERS.length);
      setPrefs((prev) => ({ ...prev, source: BUNDLED_WALLPAPERS[idx].src }));
    };
    rotate();
    timerRef.current = setInterval(rotate, ROTATE_INTERVAL);
    return () => {
      if (timerRef.current) {
        clearInterval(timerRef.current);
        timerRef.current = null;
      }
    };
  }, [prefs.autoRotate]);

  return (
    <WallpaperContext.Provider value={{ prefs, update }}>{children}</WallpaperContext.Provider>
  );
}
