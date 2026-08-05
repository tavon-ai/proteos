// formatAge renders an elapsed duration at one significant unit ("3d ago").
//
// Its own module rather than a helper inside AdminWindow: it is pure, it is the
// part of that window worth unit-testing, and co-locating a non-component
// export with a component breaks React Fast Refresh for the whole file.
//
// One unit, never "1d 4h": the console uses this to spot stale machines at a
// glance, and precision below the leading unit adds nothing to that judgement.
// The exact timestamp stays available on hover, so nothing is actually lost.
export function formatAge(deltaMs: number): string {
  // A negative delta means the server's clock is ahead of the browser's. Show
  // "just now" rather than a negative age — clock skew is not the reader's
  // problem, and "-2m ago" reads as a bug in the fleet.
  if (deltaMs < 0) return 'just now';
  const s = Math.floor(deltaMs / 1000);
  if (s < 60) return 'just now';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}
