import { describe, expect, it } from 'vitest';
import { parseCloneRef } from './repoRef';

describe('parseCloneRef', () => {
  it('accepts bare owner/repo as a GitHub full-name', () => {
    expect(parseCloneRef('octocat/hello')).toEqual({
      display: 'octocat/hello',
      fullName: 'octocat/hello',
    });
  });

  it('normalizes github.com URL forms to a full-name ref', () => {
    const want = { display: 'octocat/hello', fullName: 'octocat/hello' };
    expect(parseCloneRef('https://github.com/octocat/hello')).toEqual(want);
    expect(parseCloneRef('https://github.com/octocat/hello.git')).toEqual(want);
    expect(parseCloneRef('https://github.com/octocat/hello/')).toEqual(want);
    expect(parseCloneRef('http://github.com/octocat/hello')).toEqual(want);
    expect(parseCloneRef('git@github.com:octocat/hello.git')).toEqual(want);
    expect(parseCloneRef('github.com/octocat/hello')).toEqual(want);
    expect(parseCloneRef('  octocat/hello  ')).toEqual(want);
  });

  it('turns non-GitHub https URLs into a url ref displayed with the host', () => {
    expect(parseCloneRef('https://codeberg.org/octocat/hello')).toEqual({
      display: 'codeberg.org/octocat/hello',
      url: 'https://codeberg.org/octocat/hello',
    });
    expect(parseCloneRef('https://Codeberg.org/octocat/hello.git/')).toEqual({
      display: 'codeberg.org/octocat/hello',
      url: 'https://codeberg.org/octocat/hello',
    });
    expect(parseCloneRef('https://git.example.com:3000/octocat/hello')).toEqual({
      display: 'git.example.com:3000/octocat/hello',
      url: 'https://git.example.com:3000/octocat/hello',
    });
  });

  it('rejects everything else', () => {
    expect(parseCloneRef('../etc/passwd')).toBeNull();
    expect(parseCloneRef('owner/..')).toBeNull();
    expect(parseCloneRef('https://codeberg.org/owner/..')).toBeNull();
    expect(parseCloneRef('owner/repo/extra')).toBeNull();
    expect(parseCloneRef('https://codeberg.org/owner/repo/extra')).toBeNull();
    expect(parseCloneRef('http://codeberg.org/owner/repo')).toBeNull(); // https only for url refs
    expect(parseCloneRef('git@codeberg.org:owner/repo.git')).toBeNull();
    expect(parseCloneRef('not-a-repo')).toBeNull();
    expect(parseCloneRef('')).toBeNull();
  });
});
