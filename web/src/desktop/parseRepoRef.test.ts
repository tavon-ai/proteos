import { describe, expect, it } from 'vitest';
import { parseRepoRef } from './repoRef';

describe('parseRepoRef', () => {
  it('accepts a bare owner/repo', () => {
    expect(parseRepoRef('octocat/hello')).toBe('octocat/hello');
  });

  it('normalizes https URLs, with or without .git and trailing slash', () => {
    expect(parseRepoRef('https://github.com/octocat/hello')).toBe('octocat/hello');
    expect(parseRepoRef('https://github.com/octocat/hello.git')).toBe('octocat/hello');
    expect(parseRepoRef('https://github.com/octocat/hello/')).toBe('octocat/hello');
    expect(parseRepoRef('http://github.com/octocat/hello')).toBe('octocat/hello');
  });

  it('normalizes scp-style and host-prefixed refs', () => {
    expect(parseRepoRef('git@github.com:octocat/hello.git')).toBe('octocat/hello');
    expect(parseRepoRef('github.com/octocat/hello')).toBe('octocat/hello');
  });

  it('trims surrounding whitespace', () => {
    expect(parseRepoRef('  octocat/hello  ')).toBe('octocat/hello');
  });

  it('rejects traversal, nested paths, and non-GitHub input', () => {
    expect(parseRepoRef('../etc/passwd')).toBeNull();
    expect(parseRepoRef('owner/repo/extra')).toBeNull();
    expect(parseRepoRef('not-a-repo')).toBeNull();
    expect(parseRepoRef('')).toBeNull();
    expect(parseRepoRef('https://gitlab.com/owner/repo')).toBeNull();
  });
});
