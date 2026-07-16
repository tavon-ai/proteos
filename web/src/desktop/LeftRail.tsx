import { useRef, useState } from 'react';
import type { Me } from '../api/client';
import { useSelectedMachine } from './selectedMachineStore';
import { useWindowManager } from './windowManagerContext';
import {
  openAppLogs,
  openHomeAgent,
  openHomeTerminal,
  openLogs,
  openProjects,
  openSessions,
  openSettings,
} from './openers';
import { focusedWindow, type WindowState } from './windowState';
import { useClickOutside } from './useClickOutside';

// LeftRail is the desktop's primary navigation: a persistent 76px column of
// labeled icon buttons (Projects/Terminal/Agents/Activity/Logs/Sessions, then
// Settings and the account avatar pinned to the bottom). Clicking an item
// opens or focuses that window kind for the active machine via the shared
// openers; the item matching the focused window's kind is highlighted with
// the section's dock-kind color. Labels are always visible, so there are no
// tooltips.

// Each rail section: the window kinds that light it up, and how to open it.
// Projects/Terminal/Agents act on the ACTIVE machine (disabled when none);
// Activity, Logs, Sessions, and Settings are global windows.
interface Section {
  key: string;
  label: string;
  kinds: WindowState['kind'][];
  needsMachine: boolean;
  icon: React.ReactNode;
}

const SECTIONS: Section[] = [
  {
    key: 'projects',
    label: 'Projects',
    kinds: ['projects'],
    needsMachine: true,
    icon: (
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden>
        <path
          d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2V7z"
          stroke="currentColor"
          strokeWidth="1.7"
        />
      </svg>
    ),
  },
  {
    key: 'terminal',
    label: 'Terminal',
    kinds: ['terminal'],
    needsMachine: true,
    icon: (
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden>
        <rect x="3" y="4" width="18" height="16" rx="2" stroke="currentColor" strokeWidth="1.7" />
        <path
          d="M7 9l3 3-3 3M13 15h4"
          stroke="currentColor"
          strokeWidth="1.7"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    ),
  },
  {
    key: 'agents',
    label: 'Agents',
    kinds: ['agent'],
    needsMachine: true,
    icon: (
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden>
        <circle cx="7" cy="6" r="2.4" stroke="currentColor" strokeWidth="1.7" />
        <circle cx="7" cy="18" r="2.4" stroke="currentColor" strokeWidth="1.7" />
        <circle cx="17" cy="12" r="2.4" stroke="currentColor" strokeWidth="1.7" />
        <path
          d="M7 8.4v7.2M9.2 6.6C13 6.6 14.6 9 14.6 12"
          stroke="currentColor"
          strokeWidth="1.7"
        />
      </svg>
    ),
  },
  {
    key: 'activity',
    label: 'Activity',
    kinds: ['logs'],
    needsMachine: false,
    icon: (
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden>
        <path
          d="M3 12h4l2 6 4-12 2 6h6"
          stroke="currentColor"
          strokeWidth="1.7"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    ),
  },
  {
    key: 'applogs',
    label: 'Logs',
    kinds: ['applogs'],
    needsMachine: false,
    icon: (
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden>
        <path d="M5 4h14v16H5z" stroke="currentColor" strokeWidth="1.7" strokeLinejoin="round" />
        <path
          d="M8 9h8M8 13h8M8 17h4"
          stroke="currentColor"
          strokeWidth="1.7"
          strokeLinecap="round"
        />
      </svg>
    ),
  },
  {
    key: 'sessions',
    label: 'Sessions',
    kinds: ['sessions'],
    needsMachine: false,
    icon: (
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden>
        <circle cx="12" cy="12" r="8.4" stroke="currentColor" strokeWidth="1.7" />
        <path
          d="M12 7.4V12l3.2 2.2"
          stroke="currentColor"
          strokeWidth="1.7"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    ),
  },
];

const SETTINGS_ICON = (
  <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden>
    <path
      d="M4 7h10M18 7h2M4 17h2M10 17h10"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
    />
    <circle cx="16" cy="7" r="2.4" stroke="currentColor" strokeWidth="1.7" />
    <circle cx="8" cy="17" r="2.4" stroke="currentColor" strokeWidth="1.7" />
  </svg>
);

export function LeftRail({
  me,
  onLogout,
  loggingOut,
}: {
  me: Me;
  onLogout: () => void;
  loggingOut: boolean;
}) {
  const wm = useWindowManager();
  const { selectedId } = useSelectedMachine();
  const [accountOpen, setAccountOpen] = useState(false);
  const avatarRef = useRef<HTMLButtonElement | null>(null);
  const accountMenuRef = useRef<HTMLDivElement | null>(null);
  useClickOutside(accountOpen, [avatarRef, accountMenuRef], () => setAccountOpen(false));

  const focusedKind = focusedWindow(wm.windows, selectedId)?.kind;
  const activeKey =
    [...SECTIONS, { key: 'settings', kinds: ['settings'] as WindowState['kind'][] }].find(
      (s) => focusedKind && s.kinds.includes(focusedKind),
    )?.key ?? null;

  const openSection = (key: string) => {
    switch (key) {
      case 'projects':
        if (selectedId) openProjects(wm, selectedId);
        break;
      case 'terminal':
        if (selectedId) openHomeTerminal(wm, selectedId);
        break;
      case 'agents':
        if (selectedId) openHomeAgent(wm, selectedId);
        break;
      case 'activity':
        openLogs(wm);
        break;
      case 'applogs':
        openAppLogs(wm);
        break;
      case 'sessions':
        openSessions(wm);
        break;
    }
  };

  const item = (s: Section) => {
    const disabled = s.needsMachine && !selectedId;
    return (
      <button
        key={s.key}
        className={
          `rail-item rail-${s.key}` +
          (activeKey === s.key ? ' rail-item-active' : '') +
          (disabled ? ' rail-item-disabled' : '')
        }
        onClick={() => !disabled && openSection(s.key)}
        disabled={disabled}
        aria-current={activeKey === s.key ? 'true' : undefined}
      >
        {s.icon}
        <span className="rail-label">{s.label}</span>
        {activeKey === s.key && <span className="rail-accent" aria-hidden />}
      </button>
    );
  };

  return (
    <nav className="rail" aria-label="Primary">
      {/* Brand mark: the Proteus wave. */}
      <div className="rail-brand" aria-hidden>
        <svg width="19" height="19" viewBox="0 0 24 24" fill="none">
          <path
            d="M3 14c3-3 6-3 9 0s6 3 9 0M3 8c3-3 6-3 9 0s6 3 9 0"
            stroke="#fff"
            strokeWidth="1.9"
            strokeLinecap="round"
          />
        </svg>
      </div>

      {SECTIONS.map(item)}

      <div className="rail-bottom">
        <button
          className={
            'rail-item rail-item-settings rail-settings' +
            (activeKey === 'settings' ? ' rail-item-active' : '')
          }
          onClick={() => openSettings(wm)}
          aria-current={activeKey === 'settings' ? 'true' : undefined}
        >
          {SETTINGS_ICON}
          <span className="rail-label">Settings</span>
          {activeKey === 'settings' && <span className="rail-accent" aria-hidden />}
        </button>

        <button
          className="rail-avatar"
          ref={avatarRef}
          onClick={() => setAccountOpen((v) => !v)}
          aria-haspopup="menu"
          aria-expanded={accountOpen}
          title={me.user.login}
        >
          {me.user.avatar_url && <img src={me.user.avatar_url} alt="" width={30} height={30} />}
        </button>
      </div>

      {accountOpen && (
        <div className="rail-account-menu" role="menu" ref={accountMenuRef}>
          <div className="rail-account-user">{me.user.login}</div>
          <div className="machine-menu-sep" />
          <button
            className="machine-menu-action"
            onClick={onLogout}
            disabled={loggingOut}
            role="menuitem"
          >
            {loggingOut ? 'Signing out…' : 'Sign out'}
          </button>
        </div>
      )}
    </nav>
  );
}
