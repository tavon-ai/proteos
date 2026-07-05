import { describe, expect, it } from 'vitest';
import { shouldRedirectToMobile } from './mobileGate';

function memStorage(initial: Record<string, string> = {}) {
  const map = new Map(Object.entries(initial));
  return {
    getItem: (k: string) => map.get(k) ?? null,
    setItem: (k: string, v: string) => void map.set(k, v),
    removeItem: (k: string) => void map.delete(k),
    dump: () => Object.fromEntries(map),
  };
}

describe('shouldRedirectToMobile', () => {
  it('redirects a phone viewport hitting the bare root', () => {
    expect(shouldRedirectToMobile('', true, memStorage())).toBe(true);
  });

  it('leaves desktop viewports alone', () => {
    expect(shouldRedirectToMobile('', false, memStorage())).toBe(false);
  });

  it('?desktop=1 forces desktop on a phone and remembers the choice', () => {
    const storage = memStorage();
    expect(shouldRedirectToMobile('?desktop=1', true, storage)).toBe(false);
    // Next plain "/" visit honours the remembered choice.
    expect(shouldRedirectToMobile('', true, storage)).toBe(false);
  });

  it('?desktop=0 forgets the remembered choice', () => {
    const storage = memStorage({ 'proteos-ui-choice': 'desktop' });
    expect(shouldRedirectToMobile('?desktop=0', true, storage)).toBe(true);
    expect(shouldRedirectToMobile('', true, storage)).toBe(true);
  });

  it('the remembered choice never affects desktop viewports', () => {
    expect(shouldRedirectToMobile('', false, memStorage({ 'proteos-ui-choice': 'desktop' }))).toBe(
      false,
    );
  });
});
