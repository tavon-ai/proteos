import type { Project } from "../api/client";
import type { WindowManagerContext } from "./WindowManager";

// openers builds the OpenSpec for each window kind and hands it to the window
// manager. Centralizing this keeps the launcher, taskbar, and dock consistent:
// every project-scoped window is born already pointed at its repo folder, with a
// fresh opaque session id the saved layout reconnects to (Phase 9 decisions #1/#3).

// freshSession returns an opaque per-window session id matching the guest's
// ^[a-z0-9-]{1,32}$ constraint. It is stable for the window's lifetime (stored in
// the layout) so a reload reconnects to the same live PTY.
export function freshSession(): string {
  return "w-" + Math.random().toString(36).slice(2, 10) + Math.random().toString(36).slice(2, 6);
}

// shortName trims a project name for a window title.
function projectLabel(project: Project): string {
  return project.name;
}

export function openTerminal(wm: WindowManagerContext, project: Project): void {
  const session = freshSession();
  wm.open({
    id: session,
    kind: "terminal",
    title: `Terminal — ${projectLabel(project)}`,
    projectId: project.path,
    session,
    cwd: project.path,
  });
}

export function openAgent(wm: WindowManagerContext, project: Project, providerKey: string, providerName: string): void {
  const session = freshSession();
  wm.open({
    id: session,
    kind: "agent",
    title: `${providerName} — ${projectLabel(project)}`,
    projectId: project.path,
    session,
    provider: providerKey,
    cwd: project.path,
  });
}

export function openEditor(wm: WindowManagerContext, project: Project): void {
  wm.open({
    id: "editor-" + project.path,
    kind: "editor",
    title: `Editor — ${projectLabel(project)}`,
    projectId: project.path,
    folder: project.path,
    dedupeKey: project.path,
  });
}

export function openSettings(wm: WindowManagerContext): void {
  wm.open({ id: "settings", kind: "settings", title: "Settings", dedupeKey: "settings" });
}

export function openLogs(wm: WindowManagerContext): void {
  wm.open({ id: "logs", kind: "logs", title: "Activity", dedupeKey: "logs" });
}

export function openProjects(wm: WindowManagerContext): void {
  wm.open({ id: "projects", kind: "projects", title: "Projects", dedupeKey: "projects" });
}
