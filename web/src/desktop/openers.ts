import type { Project } from '../api/client';
import type { WindowManagerContext } from './windowManagerContext';

// openers builds the OpenSpec for each window kind and hands it to the window
// manager. Centralizing this keeps the launcher, taskbar, and dock consistent:
// every project-scoped window is born already pointed at its repo folder, with a
// fresh opaque session id the saved layout reconnects to (Phase 9 decisions #1/#3).

// freshSession returns an opaque per-window session id matching the guest's
// ^[a-z0-9-]{1,32}$ constraint. It is stable for the window's lifetime (stored in
// the layout) so a reload reconnects to the same live PTY.
function freshSession(): string {
  return 'w-' + Math.random().toString(36).slice(2, 10) + Math.random().toString(36).slice(2, 6);
}

// shortName trims a project name for a window title.
function projectLabel(project: Project): string {
  return project.name;
}

export function openTerminal(wm: WindowManagerContext, machineId: string, project: Project): void {
  const session = freshSession();
  wm.open({
    id: session,
    kind: 'terminal',
    title: `Terminal — ${projectLabel(project)}`,
    machineId,
    projectId: project.path,
    session,
    cwd: project.path,
  });
}

// openHomeTerminal opens a terminal in the user's home directory. Unlike
// openTerminal it is not scoped to a project (no cwd ⇒ the guest lands in $HOME),
// so it works even with no repos cloned — the way to get a shell on a fresh or
// misbehaving machine. A fresh session each time, so repeated clicks open
// independent shells (matching openTerminal; no dedupe).
export function openHomeTerminal(wm: WindowManagerContext, machineId: string): void {
  const session = freshSession();
  wm.open({
    id: session,
    kind: 'terminal',
    title: 'Terminal — home',
    machineId,
    session,
  });
}

// openHomeAgent launches a coding-agent terminal in the user's home directory
// (the left rail's Agents item / the ⌘K palette). Like openHomeTerminal it is
// not project-scoped, so it works with no repos cloned. The provider is fixed
// to Claude — the default coding agent; a per-project, provider-picking
// openAgent (below) stays for a future launcher flow.
export function openHomeAgent(wm: WindowManagerContext, machineId: string): void {
  const session = freshSession();
  wm.open({
    id: session,
    kind: 'agent',
    title: 'Claude — home',
    machineId,
    session,
    provider: 'claude',
  });
}

// export function openAgent(
//   wm: WindowManagerContext,
//   machineId: string,
//   project: Project,
//   providerKey: string,
//   providerName: string,
// ): void {
//   const session = freshSession();
//   wm.open({
//     id: session,
//     kind: 'agent',
//     title: `${providerName} — ${projectLabel(project)}`,
//     machineId,
//     projectId: project.path,
//     session,
//     provider: providerKey,
//     cwd: project.path,
//   });
// }

// openChanges opens the worktree-review window for a project (GR1): one window
// per (machine, project), so reopening focuses the existing one. It is ephemeral
// (not persisted in the saved layout) — the diff is fetched live on open.
export function openChanges(wm: WindowManagerContext, machineId: string, project: Project): void {
  wm.open({
    id: `changes-${machineId}-${project.path}`,
    kind: 'changes',
    title: `Changes — ${projectLabel(project)}`,
    machineId,
    projectId: project.path,
    dedupeKey: `${machineId}|${project.path}`,
  });
}

// openTasks opens the headless task lane for a project (AT1/AT2): one window per
// (machine, project), so reopening focuses the existing one. Ephemeral (not
// persisted) — the task list + live event stream are fetched on open.
export function openTasks(wm: WindowManagerContext, machineId: string, project: Project): void {
  wm.open({
    id: `tasks-${machineId}-${project.path}`,
    kind: 'tasks',
    title: `Tasks — ${projectLabel(project)}`,
    machineId,
    projectId: project.path,
    dedupeKey: `${machineId}|${project.path}`,
  });
}

export function openEditor(wm: WindowManagerContext, machineId: string, project: Project): void {
  wm.open({
    id: `editor-${machineId}-${project.path}`,
    kind: 'editor',
    title: `Editor — ${projectLabel(project)}`,
    machineId,
    projectId: project.path,
    folder: project.path,
    dedupeKey: `${machineId}|${project.path}`,
  });
}

// openPreview opens a window framing the machine's app on the given port (PP3).
// One window per (machine, port): repeat opens of the same port focus the
// existing window, while a different port opens its own — so several previews
// coexist and switching ports leaves the others untouched.
export function openPreview(wm: WindowManagerContext, machineId: string, port: number): void {
  wm.open({
    id: `preview-${machineId}-${port}`,
    kind: 'preview',
    title: `App — port ${port}`,
    machineId,
    port,
    dedupeKey: `${machineId}|${port}`,
  });
}

export function openSettings(wm: WindowManagerContext): void {
  wm.open({ id: 'settings', kind: 'settings', title: 'Settings', dedupeKey: 'settings' });
}

export function openLogs(wm: WindowManagerContext): void {
  wm.open({ id: 'logs', kind: 'logs', title: 'Activity', dedupeKey: 'logs' });
}

// openAppLogs opens the Proteos application logs window (TAV-108): API + UI
// logs, distinct from the machine-activity feed above. Global window (no
// machine scope), one singleton like Settings/Activity.
export function openAppLogs(wm: WindowManagerContext): void {
  wm.open({ id: 'app-logs', kind: 'applogs', title: 'Logs', dedupeKey: 'applogs' });
}

export function openProjects(wm: WindowManagerContext, machineId: string): void {
  wm.open({
    id: `projects-${machineId}`,
    kind: 'projects',
    title: 'Projects',
    machineId,
    dedupeKey: `projects|${machineId}`,
  });
}
