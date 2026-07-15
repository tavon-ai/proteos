import { useState } from 'react';
import { ProvidersPanel } from '../components/ProvidersPanel';
import { ClaudeSubscriptionPanel } from '../components/ClaudeSubscriptionPanel';
import { GitSshPanel } from '../components/GitSshPanel';
import { TokensPanel } from '../components/TokensPanel';
import { DownloadsPanel } from '../components/DownloadsPanel';
import { WallpaperPanel } from '../components/WallpaperPanel';
import { GitHubStatus } from '../components/GitHubStatus';
import { GitHostsPanel } from '../components/GitHostsPanel';
import { reconnectRequired, useRepos } from '../api/hooks';

type Tab = 'providers' | 'claude' | 'gitssh' | 'github' | 'tokens' | 'downloads' | 'wallpaper';

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
          aria-selected={tab === 'claude'}
          className={tab === 'claude' ? 'settings-tab active' : 'settings-tab'}
          onClick={() => setTab('claude')}
        >
          Claude subscription
        </button>
        <button
          role="tab"
          aria-selected={tab === 'gitssh'}
          className={tab === 'gitssh' ? 'settings-tab active' : 'settings-tab'}
          onClick={() => setTab('gitssh')}
        >
          Git &amp; SSH
        </button>
        <button
          role="tab"
          aria-selected={tab === 'github'}
          className={tab === 'github' ? 'settings-tab active' : 'settings-tab'}
          onClick={() => setTab('github')}
        >
          Git hosting
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
        <button
          role="tab"
          aria-selected={tab === 'wallpaper'}
          className={tab === 'wallpaper' ? 'settings-tab active' : 'settings-tab'}
          onClick={() => setTab('wallpaper')}
        >
          Wallpaper
        </button>
      </div>
      <div className="settings-body">
        {tab === 'providers' && <ProvidersPanel />}
        {tab === 'claude' && <ClaudeSubscriptionPanel />}
        {tab === 'gitssh' && <GitSshPanel />}
        {tab === 'github' && <GitHubTab />}
        {tab === 'tokens' && <TokensPanel />}
        {tab === 'downloads' && <DownloadsPanel />}
        {tab === 'wallpaper' && <WallpaperPanel />}
      </div>
    </div>
  );
}

function GitHubTab() {
  const { data, error } = useRepos();
  const reconnect = reconnectRequired(error);
  return (
    <>
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
      {/* Additional git hosts (Gitea/Forgejo): hidden when none are configured. */}
      <GitHostsPanel />
    </>
  );
}
