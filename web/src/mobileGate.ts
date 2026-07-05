// mobileGate decides whether a visit to the root route should land on the
// purpose-built mobile shell (/m) instead of the desktop window manager
// (TAV-79). Only the bare "/" is gated: /m and the PR deep links never
// bounce, and a desktop user can still open /m manually.
//
// Escape hatch: "/?desktop=1" forces the desktop UI on a phone and remembers
// that choice for future plain "/" visits; "/?desktop=0" forgets it.

export const PHONE_MEDIA_QUERY = '(max-width: 768px) and (pointer: coarse)';

const CHOICE_KEY = 'proteos-ui-choice';

// shouldRedirectToMobile is the pure decision; App.tsx feeds it the live
// location.search, matchMedia result, and localStorage.
export function shouldRedirectToMobile(
  search: string,
  isPhoneViewport: boolean,
  storage: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>,
): boolean {
  const desktop = new URLSearchParams(search).get('desktop');
  if (desktop === '1') {
    storage.setItem(CHOICE_KEY, 'desktop');
    return false;
  }
  if (desktop === '0') {
    storage.removeItem(CHOICE_KEY);
  } else if (storage.getItem(CHOICE_KEY) === 'desktop') {
    return false;
  }
  return isPhoneViewport;
}
