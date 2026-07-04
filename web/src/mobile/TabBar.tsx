import { MonitorIcon, NodesIcon } from './icons';

export type MobileTab = 'machines' | 'review';

// TabBar is the fixed bottom navigation: Machines ↔ Review. (No Activity tab —
// deliberately absent until there is something to show.)
export function TabBar({ tab, onSelect }: { tab: MobileTab; onSelect: (t: MobileTab) => void }) {
  return (
    <nav className="m-tabbar" aria-label="Sections">
      <TabButton
        label="Machines"
        active={tab === 'machines'}
        icon={<MonitorIcon size={23} />}
        onClick={() => onSelect('machines')}
      />
      <TabButton
        label="Review"
        active={tab === 'review'}
        icon={<NodesIcon size={23} />}
        onClick={() => onSelect('review')}
      />
    </nav>
  );
}

function TabButton({
  label,
  active,
  icon,
  onClick,
}: {
  label: string;
  active: boolean;
  icon: React.ReactNode;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      className={`m-tab${active ? ' is-active' : ''}`}
      aria-current={active ? 'page' : undefined}
      onClick={onClick}
    >
      {icon}
      <span className="m-tab-label">{label}</span>
    </button>
  );
}
