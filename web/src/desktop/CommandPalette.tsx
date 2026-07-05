import { useMemo, useState, type ReactNode } from 'react';
import { useProjects } from '../api/hooks';
import { useSelectedMachine } from './selectedMachineStore';
import { useWindowManager } from './windowManagerContext';
import { openEditor, openHomeAgent, openHomeTerminal, openProjects } from './openers';

// CommandPalette is the ⌘K surface over the window area: a filterable list of
// Actions (clone, terminal, agent, app preview — all on the active machine)
// and the machine's cloned Projects. Selecting an entry opens the matching
// window through the shared openers and closes the palette. Rendered inside
// the window surface so it never covers the rail or context bar.

interface Entry {
  id: string;
  section: 'Actions' | 'Projects';
  label: string;
  hint?: string;
  mono?: boolean;
  icon: ReactNode;
  run: () => void;
}

const DOT = (color: string) => (
  <span className="palette-dot" style={{ background: color }} aria-hidden />
);

export function CommandPalette({
  onClose,
  onOpenApp,
}: {
  onClose: () => void;
  /** Open the "app on port" dialog (the palette closes first). */
  onOpenApp: () => void;
}) {
  const wm = useWindowManager();
  const { selected, selectedId } = useSelectedMachine();
  const running = selected?.state === 'running';
  const { data } = useProjects(selectedId, running);
  const [query, setQuery] = useState('');
  const [cursor, setCursor] = useState(0);

  const entries = useMemo<Entry[]>(() => {
    const list: Entry[] = [];
    if (selectedId) {
      const onMachine = selected ? `on ${selected.name}` : undefined;
      list.push(
        {
          id: 'clone',
          section: 'Actions',
          label: 'Clone a repo…',
          hint: onMachine,
          icon: (
            <svg width="17" height="17" viewBox="0 0 24 24" fill="none" aria-hidden>
              <path
                d="M6 3v6M3 6h6M14 5l3 3M12 14l6 6M4 20l6-6"
                stroke="#2f81f7"
                strokeWidth="1.7"
                strokeLinecap="round"
              />
            </svg>
          ),
          run: () => openProjects(wm, selectedId),
        },
        {
          id: 'terminal',
          section: 'Actions',
          label: 'Open terminal',
          hint: onMachine,
          icon: (
            <svg width="17" height="17" viewBox="0 0 24 24" fill="none" aria-hidden>
              <rect x="3" y="4" width="18" height="16" rx="2" stroke="#3fb950" strokeWidth="1.7" />
              <path
                d="M7 9l3 3-3 3"
                stroke="#3fb950"
                strokeWidth="1.7"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          ),
          run: () => openHomeTerminal(wm, selectedId),
        },
        {
          id: 'agent',
          section: 'Actions',
          label: 'Run coding agent',
          hint: onMachine,
          icon: (
            <svg width="17" height="17" viewBox="0 0 24 24" fill="none" aria-hidden>
              <circle cx="7" cy="6" r="2.4" stroke="#d29922" strokeWidth="1.7" />
              <circle cx="7" cy="18" r="2.4" stroke="#d29922" strokeWidth="1.7" />
              <circle cx="17" cy="12" r="2.4" stroke="#d29922" strokeWidth="1.7" />
              <path
                d="M7 8.4v7.2M9.2 6.6C13 6.6 14.6 9 14.6 12"
                stroke="#d29922"
                strokeWidth="1.7"
              />
            </svg>
          ),
          run: () => openHomeAgent(wm, selectedId),
        },
        {
          id: 'open-app',
          section: 'Actions',
          label: 'Open app on port…',
          hint: onMachine,
          icon: (
            <svg width="17" height="17" viewBox="0 0 24 24" fill="none" aria-hidden>
              <rect x="3" y="5" width="18" height="14" rx="2" stroke="#2f81f7" strokeWidth="1.7" />
              <path d="M3 9h18" stroke="#2f81f7" strokeWidth="1.7" />
            </svg>
          ),
          run: onOpenApp,
        },
      );
      for (const p of data?.projects ?? []) {
        list.push({
          id: `project-${p.path}`,
          section: 'Projects',
          label: p.name,
          hint: 'open',
          mono: true,
          icon: DOT('#db61a2'),
          run: () => openEditor(wm, selectedId, p),
        });
      }
    }
    const q = query.trim().toLowerCase();
    return q ? list.filter((e) => e.label.toLowerCase().includes(q)) : list;
  }, [wm, selected, selectedId, data, query, onOpenApp]);

  const active = Math.min(cursor, Math.max(0, entries.length - 1));

  const pick = (e: Entry) => {
    onClose();
    e.run();
  };

  const onKeyDown = (ev: React.KeyboardEvent) => {
    if (ev.key === 'Escape') {
      onClose();
    } else if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      setCursor(Math.min(active + 1, entries.length - 1));
    } else if (ev.key === 'ArrowUp') {
      ev.preventDefault();
      setCursor(Math.max(active - 1, 0));
    } else if (ev.key === 'Enter' && entries[active]) {
      pick(entries[active]);
    }
  };

  let lastSection: string | null = null;

  return (
    <div className="palette-backdrop" onClick={onClose}>
      <div
        className="palette"
        role="dialog"
        aria-label="Command palette"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="palette-head">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" aria-hidden>
            <circle cx="11" cy="11" r="7" stroke="#8b949e" strokeWidth="1.8" />
            <path d="M20 20l-3.5-3.5" stroke="#8b949e" strokeWidth="1.8" strokeLinecap="round" />
          </svg>
          <input
            className="palette-input"
            value={query}
            placeholder={selectedId ? 'Search actions and projects…' : 'No machine selected'}
            autoFocus
            onChange={(e) => {
              setQuery(e.target.value);
              setCursor(0);
            }}
            onKeyDown={onKeyDown}
          />
          <kbd className="palette-esc">esc</kbd>
        </div>
        <div className="palette-list">
          {entries.length === 0 && <div className="palette-empty">No matches</div>}
          {entries.map((e, i) => {
            const header = e.section !== lastSection ? e.section : null;
            lastSection = e.section;
            return (
              <div key={e.id}>
                {header && <div className="palette-section">{header}</div>}
                <button
                  className={'palette-item' + (i === active ? ' palette-item-active' : '')}
                  onMouseEnter={() => setCursor(i)}
                  onClick={() => pick(e)}
                >
                  {e.icon}
                  <span className={'palette-item-label' + (e.mono ? ' mono' : '')}>{e.label}</span>
                  {e.hint && <span className="palette-item-hint">{e.hint}</span>}
                </button>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
