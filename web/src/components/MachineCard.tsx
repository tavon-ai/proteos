import { useState } from "react";
import type { MachineState, MachineSummary } from "../api/client";
import { useMachine, useMachineEvents, useMachineMutations } from "../api/hooks";
import { TerminalPanel } from "./TerminalPanel";

// Transitional states show a spinner and disable action buttons.
const TRANSITIONAL: ReadonlySet<MachineState> = new Set([
  "requested",
  "provisioning",
  "starting",
  "stopping",
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
  const [terminalOpen, setTerminalOpen] = useState(false);

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

      <EventLog events={events} />

      {terminalOpen && machine.state === "running" && (
        <TerminalPanel machineID={machine.id} onClose={() => setTerminalOpen(false)} />
      )}
    </section>
  );
}

function StateBadge({ state }: { state: MachineState }) {
  return <span className={`badge badge-${state}`}>{state}</span>;
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
