// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { canAttemptSignIn, clearSignInAttempt, markSignInAttempted, safeNext } from './autoSignIn';

// Captured before any test can replace it with a throwing getter.
const realSessionStorage = window.sessionStorage;

beforeEach(() => {
  window.sessionStorage.clear();
});

afterEach(() => {
  // Not just tidiness: storage left broken silently disables auto sign-in for
  // every test after it, and they would pass for the wrong reason.
  Object.defineProperty(window, 'sessionStorage', {
    configurable: true,
    value: realSessionStorage,
  });
});

describe('auto sign-in guard', () => {
  it('allows the first attempt and refuses the second', () => {
    expect(canAttemptSignIn()).toBe(true);
    markSignInAttempted();
    expect(canAttemptSignIn()).toBe(false);
  });

  // A resolved session clears the guard, so a session that expires an hour from
  // now gets its own silent attempt instead of inheriting this one's.
  it('allows a fresh attempt once a session has resolved', () => {
    markSignInAttempted();
    clearSignInAttempt();
    expect(canAttemptSignIn()).toBe(true);
  });

  // Without somewhere to record the attempt there is no loop protection, so the
  // safe answer is "no" — a wasted click beats an infinite bounce between here
  // and Zitadel. Modelled on Safari private mode, where the property access
  // itself throws.
  it('refuses when storage is unavailable', () => {
    Object.defineProperty(window, 'sessionStorage', {
      configurable: true,
      get() {
        throw new Error('storage blocked');
      },
    });

    expect(canAttemptSignIn()).toBe(false);
    // And the writes must not throw either, or the caller crashes instead of
    // falling back to the button.
    expect(() => markSignInAttempted()).not.toThrow();
    expect(() => clearSignInAttempt()).not.toThrow();
  });
});

// The client half of the open-redirect guard. The server sanitizes again; this
// exists so a bad value never leaves the browser.
describe('safeNext', () => {
  it('keeps same-origin paths', () => {
    expect(safeNext('/m')).toBe('/m');
    expect(safeNext('/m/abc/pr/7')).toBe('/m/abc/pr/7');
    expect(safeNext('/ok?x=1')).toBe('/ok?x=1');
  });

  it('collapses anything that could leave the origin', () => {
    expect(safeNext(null)).toBe('/');
    expect(safeNext('')).toBe('/');
    expect(safeNext('//evil.example')).toBe('/');
    expect(safeNext('/\\evil.example')).toBe('/');
    expect(safeNext('https://evil.example')).toBe('/');
    expect(safeNext('javascript:alert(1)')).toBe('/');
    expect(safeNext('evil.example')).toBe('/');
  });
});
