import { useState } from "react";
import type { MachineState, MachineSummary } from "../api/client";
import {
  useMachine,
  useMachineEvents,
  useMachineMutations,
  useProviders,
} from "../api/hooks";
import { TerminalPanel } from "./TerminalPanel";

// Transitional states show a spinner and disable action buttons. hibernating is
// the Phase 4 stop=hibernate path (running → hibernating → stopped).
const TRANSITIONAL: ReadonlySet<MachineState> = new Set([
  "requested",
  "provisioning",
  "starting",
  "stopping",
  "hibernating",
]);

function isTransitional(s: MachineState): boolean {
  return TRANSITIONAL.has(s);
}

// MachineCard renders the user's machine: a live state badge, the actions valid
// for the current state, a last_error banner, and a rolling event log. State is
// kept live by the SSE stream (useMachineEvents writes into the same query
// cache useMachine reads).
export function MachineCard({ initialMachine }: { initialMachine: MachineSummary | null }) {
  const { data: machine } = useMachine(initialMachine);
  const events = useMachineEvents();
  const { create, start, stop } = useMachineMutations();
  const { data: providers } = useProviders();
  const [terminalOpen, setTerminalOpen] = useState(false);
  // The registry key of the provider whose agent session is open, or null.
  const [agentProvider, setAgentProvider] = useState<string | null>(null);

  // The launch list is rendered entirely from the registry: any enabled provider
  // the user has keyed is launchable, with zero per-provider code (Phase 6 #5).
  const enabledProviders = providers?.filter((p) => p.enabled) ?? [];
  const launchable = enabledProviders.filter((p) => p.key_set);
  const needKey = enabledProviders.filter((p) => !p.key_set);
  const openProvider = providers?.find((p) => p.key === agentProvider) ?? null;

  if (!machine) {
    return (
      <section className="empty-state">
        <h2>No machine yet</h2>
        <p className="muted">
          You don't have a workspace machine yet. Creating one spins up an
          isolated environment for your AI coding agents.
        </p>
        <button className="btn" onClick={() => create.mutate()} disabled={create.isPending}>
          {create.isPending ? "Creating…" : "Create machine"}
        </button>
        {create.isError && (
          <p className="error-banner">Could not create machine. Please try again.</p>
        )}
      </section>
    );
  }

  const busy = isTransitional(machine.state) || create.isPending || start.isPending || stop.isPending;

  return (
    <section className="machine-card">
      <div className="machine-header">
        <StateBadge state={machine.state} />
        {machine.boot && <BootChip boot={machine.boot} />}
        {isTransitional(machine.state) && <span className="spinner" aria-label="working" />}
      </div>

      <dl className="machine-meta">
        <div>
          <dt>Guest IP</dt>
          <dd>{machine.guest_ip ?? "—"}</dd>
        </div>
        <div>
          <dt>Resources</dt>
          <dd>
            {machine.resource_spec.vcpus} vCPU · {machine.resource_spec.mem_mib} MiB
          </dd>
        </div>
        <div>
          <dt>Image</dt>
          <dd>
            {machine.kernel_ref} / {machine.rootfs_ref}
          </dd>
        </div>
        <div>
          <dt>Disk</dt>
          <dd>{formatDisk(machine.disk_mib ?? machine.resource_spec.disk_mib ?? null)}</dd>
        </div>
      </dl>

      {machine.state === "error" && machine.last_error && (
        <p className="error-banner" role="alert">
          {machine.last_error}
        </p>
      )}

      <div className="machine-actions">
        {machine.state === "running" && (
          <>
            <button className="btn" onClick={() => setTerminalOpen(true)}>
              Open terminal
            </button>
            {launchable.map((p) => (
              <button
                key={p.key}
                className="btn btn-primary"
                onClick={() => setAgentProvider(p.key)}
              >
                Launch {p.display_name}
              </button>
            ))}
            <button className="btn" onClick={() => stop.mutate()} disabled={busy}>
              Stop
            </button>
          </>
        )}
        {(machine.state === "stopped" || machine.state === "error") && (
          <button className="btn" onClick={() => start.mutate()} disabled={busy}>
            Start
          </button>
        )}
      </div>

      {machine.state === "running" && needKey.length > 0 && (
        <p className="muted launch-hint">
          Set an API key for {needKey.map((p) => p.display_name).join(", ")} in{" "}
          <strong>AI providers</strong> below to launch{" "}
          {needKey.length > 1 ? "them" : "it"} here.
        </p>
      )}

      <EventLog events={events} />

      {terminalOpen && machine.state === "running" && (
        <TerminalPanel machineID={machine.id} onClose={() => setTerminalOpen(false)} />
      )}
      {agentProvider && machine.state === "running" && (
        <TerminalPanel
          machineID={machine.id}
          provider={agentProvider}
          title={openProvider?.display_name ?? agentProvider}
          onClose={() => setAgentProvider(null)}
        />
      )}
    </section>
  );
}

function StateBadge({ state }: { state: MachineState }) {
  return <span className={`badge badge-${state}`}>{state}</span>;
}

// BootChip shows how the current run started: "resumed" (from a hibernation
// snapshot) or "cold". Hidden until the machine has booted at least once.
function BootChip({ boot }: { boot: "cold" | "resumed" }) {
  const label = boot === "resumed" ? "resumed" : "cold boot";
  return (
    <span className={`chip chip-boot chip-boot-${boot}`} title="How this run started">
      {label}
    </span>
  );
}

// formatDisk renders a disk size in MiB as GiB when it divides evenly, else MiB.
function formatDisk(mib: number | null): string {
  if (mib == null || mib <= 0) return "—";
  if (mib % 1024 === 0) return `${mib / 1024} GiB`;
  return `${mib} MiB`;
}

function EventLog({ events }: { events: import("../api/client").MachineEvent[] }) {
  if (events.length === 0) return null;
  return (
    <details className="event-log" open>
      <summary>Activity</summary>
      <ul>
        {events.map((e) => (
          <li key={e.id}>
            <span className="event-time">{new Date(e.created_at).toLocaleTimeString()}</span>
            <span className="event-desc">
              {e.from_state && e.to_state ? `${e.from_state} → ${e.to_state}` : e.type}
              {e.type === "error" && reasonOf(e.payload) ? `: ${reasonOf(e.payload)}` : ""}
            </span>
          </li>
        ))}
      </ul>
    </details>
  );
}

function reasonOf(payload: Record<string, unknown>): string {
  const r = payload?.["reason"];
  return typeof r === "string" ? r : "";
}
