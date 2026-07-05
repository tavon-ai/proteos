import { useEffect, type RefObject } from 'react';

// useClickOutside closes chrome dropdowns (machine pill menu, rail account
// menu). A backdrop div cannot do this job here: the rail and context bar use
// `backdrop-filter`, which makes them containing blocks for position:fixed —
// a backdrop rendered inside them pins to the bar instead of the viewport
// (same quirk Modal.tsx portals around). A document-level listener has no
// such stacking/containing-block constraints.
export function useClickOutside(
  active: boolean,
  refs: RefObject<HTMLElement | null>[],
  onOutside: () => void,
): void {
  useEffect(() => {
    if (!active) return;
    const onPointerDown = (e: PointerEvent) => {
      const target = e.target as Node;
      if (refs.some((r) => r.current?.contains(target))) return;
      onOutside();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onOutside();
    };
    document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('keydown', onKey);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, onOutside]);
}
