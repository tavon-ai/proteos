import { useState } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router-dom';
import type { Me } from '../api/client';
import { useMachineEvents, useMachines } from '../api/hooks';
import { MachinesScreen } from './MachinesScreen';
import { ReviewScreen } from './ReviewScreen';
import { TabBar, type MobileTab } from './TabBar';
import './mobile.css';

// MobileApp is the purpose-built phone shell: two screens (Review + Machines)
// behind a bottom tab bar. It exists for one dominant flow — a Telegram link
// deep-links straight to a PR at /m/:machineId/pr/:number?repo=owner/name —
// with Machines as the secondary glance-and-toggle surface. Everything else
// the desktop does is deliberately absent here.
export function MobileApp({ me }: { me: Me }) {
  const { machineId, number } = useParams();
  const [params] = useSearchParams();
  const repo = params.get('repo') ?? '';
  const prNumber = Number(number ?? 0);
  const hasReviewContext = !!repo && prNumber > 0;

  // Deep links land on Review; opening /m bare lands on Machines.
  const [tab, setTab] = useState<MobileTab>(hasReviewContext ? 'review' : 'machines');
  const navigate = useNavigate();

  const machines = useMachines(me.machines);
  useMachineEvents(); // keeps the machines cache live over SSE
  const machine = machines.data?.find((m) => m.id === machineId) ?? null;

  // The back chevron returns to wherever the link came from (Telegram, the
  // browser); a fresh deep-link with no history falls back to Machines.
  const back = () => {
    if (window.history.length > 1) navigate(-1);
    else setTab('machines');
  };

  // Both screens stay mounted (hidden via CSS) so each keeps its scroll
  // position across tab switches.
  return (
    <div className="m-app">
      <div className="m-panes">
        <div className={`m-pane${tab === 'machines' ? '' : ' is-hidden'}`}>
          <MachinesScreen machines={machines.data ?? []} />
        </div>
        <div className={`m-pane${tab === 'review' ? '' : ' is-hidden'}`}>
          <ReviewScreen
            repo={repo}
            number={prNumber}
            machine={machine}
            avatarUrl={me.user.avatar_url}
            onBack={back}
          />
        </div>
      </div>
      <TabBar tab={tab} onSelect={setTab} />
    </div>
  );
}
