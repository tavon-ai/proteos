import { useState } from 'react';
import { ProvidersPanel } from '../components/ProvidersPanel';
import { TokensPanel } from '../components/TokensPanel';
import { DownloadsPanel } from '../components/DownloadsPanel';
import { GitHubStatus } from '../components/GitHubStatus';
import { reconnectRequired, useRepos } from '../api/hooks';

type Tab = 'providers' | 'github' | 'tokens' | 'downloads';

// SettingsWindow folds the Phase 5–7 panels into one window with tabs
// (decision #7): the Providers tab manages each provider's write-only API key
// (reusing ProvidersPanel verbatim); the GitHub tab shows the connection state,
// a reconnect action when the grant is stale, and a link to manage which repos
// the App can see. The clone form itself lives on the Projects launcher.
export function SettingsWindow() {
  const [tab, setTab] = useState<Tab>('providers');
  return (
    <div className="settings-window">
      <div className="settings-tabs" role="tablist">
        <button
          role="tab"
          aria-selected={tab === 'providers'}
          className={tab === 'providers' ? 'settings-tab active' : 'settings-tab'}
          onClick={() => setTab('providers')}
        >
          AI providers
        </button>
        <button
          role="tab"
          aria-selected={tab === 'github'}
          className={tab === 'github' ? 'settings-tab active' : 'settings-tab'}
          onClick={() => setTab('github')}
        >
          GitHub
        </button>
        <button
          role="tab"
          aria-selected={tab === 'tokens'}
          className={tab === 'tokens' ? 'settings-tab active' : 'settings-tab'}
          onClick={() => setTab('tokens')}
        >
          CLI tokens
        </button>
        <button
          role="tab"
          aria-selected={tab === 'downloads'}
          className={tab === 'downloads' ? 'settings-tab active' : 'settings-tab'}
          onClick={() => setTab('downloads')}
        >
          Downloads
        </button>
      </div>
      <div className="settings-body">
        {tab === 'providers' && <ProvidersPanel />}
        {tab === 'github' && <GitHubTab />}
        {tab === 'tokens' && <TokensPanel />}
        {tab === 'downloads' && <DownloadsPanel />}
      </div>
    </div>
  );
}

function GitHubTab() {
  const { data, error } = useRepos();
  const reconnect = reconnectRequired(error);
  return (
    <section className="github-tab">
      <div className="repos-head">
        <h2>GitHub</h2>
        <GitHubStatus reconnect={reconnect} />
      </div>
      {!reconnect && (
        <p className="muted">
          ProteOS clones and pushes using your GitHub identity. Tokens are fetched on demand and
          never written to disk inside the machine.
        </p>
      )}
      {data?.grants_url && (
        <p className="muted">
          <a href={data.grants_url} target="_blank" rel="noreferrer">
            Choose which repositories ProteOS can access ↗
          </a>
        </p>
      )}
    </section>
  );
}
