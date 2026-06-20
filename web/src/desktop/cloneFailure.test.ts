import { describe, expect, it } from 'vitest';
import { looksLikeGrantFailure } from './cloneFailure';

describe('looksLikeGrantFailure', () => {
  it('matches the git access fatals a private repo produces', () => {
    expect(
      looksLikeGrantFailure(
        "fatal: could not read Username for 'https://github.com': terminal prompts disabled",
      ),
    ).toBe(true);
    expect(
      looksLikeGrantFailure(
        "fatal: Authentication failed for 'https://github.com/owner/repo.git/'",
      ),
    ).toBe(true);
    expect(looksLikeGrantFailure('remote: Repository not found.')).toBe(true);
    expect(looksLikeGrantFailure('The requested URL returned error: 403')).toBe(true);
  });

  it('is case-insensitive', () => {
    expect(looksLikeGrantFailure('AUTHENTICATION FAILED')).toBe(true);
  });

  it('does not match unrelated failures', () => {
    expect(
      looksLikeGrantFailure('fatal: unable to access: Could not resolve host: github.com'),
    ).toBe(false);
    expect(looksLikeGrantFailure('prepare workspace: permission denied')).toBe(false);
    expect(looksLikeGrantFailure('exit status 128')).toBe(false);
    expect(looksLikeGrantFailure('')).toBe(false);
  });
});
