import { Navigate, Route, Routes } from 'react-router-dom';
import { useMe } from './api/hooks';
import { SessionExpiredError, type Me } from './api/client';
import { Login } from './routes/Login';
import { Desktop } from './desktop/Desktop';
import { MobileApp } from './mobile/MobileApp';

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/" element={<RequireAuth render={(me) => <Desktop me={me} />} />} />
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
