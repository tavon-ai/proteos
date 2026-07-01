import { createContext, useContext } from 'react';

export const STORAGE_KEY = 'proteos.wallpaper';

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

export const DEFAULTS: WallpaperPrefs = {
  source: BUNDLED_WALLPAPERS[0].src,
  autoRotate: false,
};

export function loadPrefs(): WallpaperPrefs {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw) return { ...DEFAULTS, ...JSON.parse(raw) };
  } catch {
    // ignore malformed/unavailable storage; fall back to defaults
  }
  return DEFAULTS;
}

export interface WallpaperContextValue {
  prefs: WallpaperPrefs;
  update: (patch: Partial<WallpaperPrefs>) => void;
}

export const WallpaperContext = createContext<WallpaperContextValue | null>(null);

export function useWallpaper(): WallpaperContextValue {
  const ctx = useContext(WallpaperContext);
  if (!ctx) throw new Error('useWallpaper must be used inside WallpaperProvider');
  return ctx;
}
