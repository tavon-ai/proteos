import { useMemo } from 'react';
import { Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { useMe } from './api/hooks';
import { SessionExpiredError, type Me } from './api/client';
import { Login } from './routes/Login';
import { Desktop } from './desktop/Desktop';
import { MobileApp } from './mobile/MobileApp';
import { PHONE_MEDIA_QUERY, shouldRedirectToMobile } from './mobileGate';

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/" element={<RootGate />} />
      {/* Mobile shell (purpose-built phone UI). /m/:machineId/pr/:number is the
          Telegram deep-link target; /m bare lands on the Machines tab. */}
      <Route path="/m" element={<RequireAuth render={(me) => <MobileApp me={me} />} />} />
      <Route
        path="/m/:machineId/pr/:number"
        element={<RequireAuth render={(me) => <MobileApp me={me} />} />}
      />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

// RootGate sends phones hitting the bare "/" to the mobile shell (TAV-79);
// everyone else gets the desktop. Only this route is gated — deep links never
// bounce and /m stays reachable from a desktop browser. "?desktop=1" is the
// remembered escape hatch back to the desktop UI (see mobileGate.ts).
function RootGate() {
  const { search } = useLocation();
  const toMobile = useMemo(() => {
    try {
      return shouldRedirectToMobile(
        search,
        window.matchMedia(PHONE_MEDIA_QUERY).matches,
        window.localStorage,
      );
    } catch {
      // matchMedia/localStorage unavailable (odd embedder, privacy mode):
      // fail open to the desktop UI.
      return false;
    }
  }, [search]);

  if (toMobile) return <Navigate to="/m" replace />;
  return <RequireAuth render={(me) => <Desktop me={me} />} />;
}

// RequireAuth boots by querying /api/me. A 401 redirects to /login; otherwise
// the authed shell renders. This is the single auth gate for the SPA.
function RequireAuth({ render }: { render: (me: Me) => React.ReactNode }) {
  const { data, isLoading, error } = useMe();

  if (isLoading) {
    return <div className="centered">Loading…</div>;
  }
  if (error instanceof SessionExpiredError) {
    return <Navigate to="/login" replace />;
  }
  if (error) {
    return <div className="centered">Something went wrong. Please reload.</div>;
  }
  if (!data) {
    return <Navigate to="/login" replace />;
  }
  return render(data);
}
