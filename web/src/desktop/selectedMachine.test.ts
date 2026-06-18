import { describe, expect, it } from 'vitest';
import type { MachineSummary } from '../api/client';
import { chooseDefaultMachine } from './selectedMachine';

// Minimal machine factory — only id/state matter to chooseDefaultMachine.
function m(id: string, state: MachineSummary['state']): MachineSummary {
  return {
    id,
    name: id,
    state,
    guest_ip: null,
    kernel_ref: 'k',
    rootfs_ref: 'r',
    resource_spec: { vcpus: 1, mem_mib: 1 },
    last_error: null,
    created_at: '',
    boot: null,
    disk_id: null,
    disk_mib: null,
    snapshot: null,
  };
}

describe('chooseDefaultMachine', () => {
  it('returns null when there are no machines', () => {
    expect(chooseDefaultMachine([], null)).toBeNull();
    expect(chooseDefaultMachine([], 'a')).toBeNull();
  });

  it('keeps the persisted id when it still exists', () => {
    const machines = [m('a', 'stopped'), m('b', 'running')];
    expect(chooseDefaultMachine(machines, 'a')).toBe('a');
  });

  it('prefers the first running machine when the persisted id is gone', () => {
    const machines = [m('a', 'stopped'), m('b', 'running')];
    expect(chooseDefaultMachine(machines, 'ghost')).toBe('b');
  });

  it('falls back to the first machine when none are running', () => {
    const machines = [m('a', 'stopped'), m('b', 'error')];
    expect(chooseDefaultMachine(machines, null)).toBe('a');
  });
});
