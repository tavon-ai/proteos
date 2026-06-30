import { useRef, useState } from 'react';
import { BUNDLED_WALLPAPERS, useWallpaper } from '../desktop/wallpaperContext';

export function WallpaperPanel() {
  const { prefs, update } = useWallpaper();
  const [urlInput, setUrlInput] = useState('');
  const [urlError, setUrlError] = useState('');
  const fileRef = useRef<HTMLInputElement>(null);

  const handleFileUpload = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = (ev) => {
      const dataUrl = ev.target?.result as string;
      update({ source: dataUrl, autoRotate: false });
    };
    reader.readAsDataURL(file);
    // Reset so re-selecting same file triggers onChange
    e.target.value = '';
  };

  const handleUrlSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = urlInput.trim();
    if (!trimmed) return;
    try {
      new URL(trimmed);
      update({ source: trimmed, autoRotate: false });
      setUrlError('');
    } catch {
      setUrlError('Enter a valid URL (http:// or https://)');
    }
  };

  return (
    <section className="wallpaper-panel">
      <h2>Wallpaper</h2>

      <div className="wallpaper-autorotate">
        <label className="wallpaper-toggle-label">
          <input
            type="checkbox"
            checked={prefs.autoRotate}
            onChange={(e) => update({ autoRotate: e.target.checked })}
          />
          <span>Rotate wallpaper every 30 minutes</span>
        </label>
        {prefs.autoRotate && (
          <p className="muted">A random bundled wallpaper is shown and changes automatically.</p>
        )}
      </div>

      <div className="wallpaper-section">
        <h3 className="wallpaper-section-title">Bundled</h3>
        <div className="wallpaper-grid">
          {BUNDLED_WALLPAPERS.map((wp) => {
            const active = prefs.source === wp.src && !prefs.autoRotate;
            return (
              <button
                key={wp.id}
                className={`wallpaper-thumb${active ? ' selected' : ''}`}
                onClick={() => update({ source: wp.src, autoRotate: false })}
                title={wp.label}
                aria-label={`Select ${wp.label} wallpaper`}
                aria-pressed={active}
              >
                <img src={wp.src} alt={wp.label} />
                <span className="wallpaper-thumb-label">{wp.label}</span>
              </button>
            );
          })}
        </div>
      </div>

      <div className="wallpaper-section">
        <h3 className="wallpaper-section-title">Upload image</h3>
        <input
          ref={fileRef}
          type="file"
          accept="image/*"
          style={{ display: 'none' }}
          onChange={handleFileUpload}
        />
        <button className="btn-ghost" onClick={() => fileRef.current?.click()}>
          Choose file…
        </button>
        <p className="muted" style={{ marginTop: '0.4rem' }}>
          JPEG, PNG, WebP, GIF, SVG — stored in your browser only.
        </p>
      </div>

      <div className="wallpaper-section">
        <h3 className="wallpaper-section-title">Image URL</h3>
        <form className="wallpaper-url-form" onSubmit={handleUrlSubmit}>
          <input
            type="url"
            className="wallpaper-url-input"
            placeholder="https://example.com/wallpaper.jpg"
            value={urlInput}
            onChange={(e) => {
              setUrlInput(e.target.value);
              setUrlError('');
            }}
          />
          <button type="submit" className="btn-ghost">
            Apply
          </button>
        </form>
        {urlError && <span className="error-inline">{urlError}</span>}
      </div>
    </section>
  );
}
