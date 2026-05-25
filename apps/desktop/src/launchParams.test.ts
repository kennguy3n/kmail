// Tests for the launch-hash snapshot module.
//
// The contract under test:
//   1. The snapshot is captured exactly once at module load.
//   2. Subsequent mutations of `window.location.hash` (which
//      `<HashRouter>` performs at mount time) do NOT change the
//      snapshot values.
//   3. The `__reseedLaunchHashForTests` helper mutates the
//      snapshot in place so existing imports see the new values.

import { afterEach, describe, expect, it } from 'vitest';
import {
  __reseedLaunchHashForTests,
  launchHashParams,
} from './launchParams';

describe('launchHashParams', () => {
  afterEach(() => {
    // Restore clean state so a failing test doesn't leak into the
    // next one.
    __reseedLaunchHashForTests('');
  });

  it('snapshot survives mutation of window.location.hash', () => {
    __reseedLaunchHashForTests(
      'bff=https%3A%2F%2Fkmail.test&token=t1&db=%2Ftmp%2Fdb.sqlite',
    );
    // Simulate HashRouter mutating the hash to its `/` form.
    window.location.hash = '#/';
    expect(launchHashParams.get('bff')).toBe('https://kmail.test');
    expect(launchHashParams.get('token')).toBe('t1');
    expect(launchHashParams.get('db')).toBe('/tmp/db.sqlite');
  });

  it('does not include router-prefixed pseudo-keys', () => {
    __reseedLaunchHashForTests('bff=u&token=t&db=d');
    // The hash never contained a leading `/`, so the first key is
    // exactly `bff` — not `/bff`. This is the bug Devin Review
    // BUG_pr-review-job-..._0001 flagged: reading from
    // window.location.hash AFTER HashRouter mounts would surface
    // `/bff` as the first key, breaking the production launch.
    expect(launchHashParams.get('bff')).toBe('u');
    expect(launchHashParams.has('/bff')).toBe(false);
  });

  it('returns null for absent params (clean slate)', () => {
    __reseedLaunchHashForTests('');
    expect(launchHashParams.get('bff')).toBeNull();
    expect(launchHashParams.get('anything')).toBeNull();
  });

  it('strips a leading hash character if present', () => {
    __reseedLaunchHashForTests('#bff=u');
    expect(launchHashParams.get('bff')).toBe('u');
  });
});
