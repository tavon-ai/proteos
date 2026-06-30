import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';

const STORAGE_KEY = 'proteos.wallpaper';
const ROTATE_INTERVAL = 30 * 60 * 1000;

export interface WallpaperPrefs {
  source: string | null;
  autoRotate: boolean;
}

export interface Wallpaper {
  id: string;
  label: string;
  src: string;
}

export const BUNDLED_WALLPAPERS: Wallpaper[] = [
  { id: 'midnight', label: 'Midnight', src: '/wallpapers/01-midnight.svg' },
  { id: 'aurora', label: 'Aurora', src: '/wallpapers/02-aurora.svg' },
  { id: 'sunset', label: 'Sunset', src: '/wallpapers/03-sunset.svg' },
  { id: 'ocean', label: 'Ocean', src: '/wallpapers/04-ocean.svg' },
  { id: 'nebula', label: 'Nebula', src: '/wallpapers/05-nebula.svg' },
  { id: 'forest', label: 'Forest', src: '/wallpapers/06-forest.svg' },
  { id: 'ember', label: 'Ember', src: '/wallpapers/07-ember.svg' },
  { id: 'arctic', label: 'Arctic', src: '/wallpapers/08-arctic.svg' },
  { id: 'dusk', label: 'Dusk', src: '/wallpapers/09-dusk.svg' },
  { id: 'violet', label: 'Violet', src: '/wallpapers/10-violet.svg' },
  { id: 'desert', label: 'Desert', src: '/wallpapers/11-desert.svg' },
  { id: 'mint', label: 'Mint', src: '/wallpapers/12-mint.svg' },
];

const DEFAULTS: WallpaperPrefs = {
  source: BUNDLED_WALLPAPERS[0].src,
  autoRotate: false,
};

function loadPrefs(): WallpaperPrefs {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw) return { ...DEFAULTS, ...JSON.parse(raw) };
  } catch {}
  return DEFAULTS;
}

interface WallpaperContextValue {
  prefs: WallpaperPrefs;
  update: (patch: Partial<WallpaperPrefs>) => void;
}

const WallpaperContext = createContext<WallpaperContextValue | null>(null);

export function WallpaperProvider({ children }: { children: ReactNode }) {
  const [prefs, setPrefs] = useState<WallpaperPrefs>(loadPrefs);

  const update = useCallback((patch: Partial<WallpaperPrefs>) => {
    setPrefs((prev) => {
      const next = { ...prev, ...patch };
      try {
        localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
      } catch {}
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

  return <WallpaperContext.Provider value={{ prefs, update }}>{children}</WallpaperContext.Provider>;
}

export function useWallpaper(): WallpaperContextValue {
  const ctx = useContext(WallpaperContext);
  if (!ctx) throw new Error('useWallpaper must be used inside WallpaperProvider');
  return ctx;
}
