import { useEffect, useState } from 'react'
import QRCode from 'qrcode'

// Inline SVG icons — no extra dependency.
const LinkIcon = (
  <svg className="lead-icon" width="18" height="18" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
    <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
  </svg>
)

const TitleIcon = (
  <svg className="lead-icon" width="16" height="16" viewBox="0 0 24 24" fill="none"
    stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M4 7V4h16v3" />
    <path d="M9 20h6" />
    <path d="M12 4v16" />
  </svg>
)

// The official multicolor Google "G" — brand colors are required by
// Google's sign-in guidelines, so these are hardcoded.
const GoogleIcon = (
  <svg width="16" height="16" viewBox="0 0 48 48" aria-hidden="true">
    <path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z" />
    <path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z" />
    <path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z" />
    <path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z" />
  </svg>
)

// Eye icon for click count
const EyeIcon = (
  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor"
    strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z" />
    <circle cx="12" cy="12" r="3" />
  </svg>
)

// Download icon for QR download
const DownloadIcon = (
  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor"
    strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
    <polyline points="7 10 12 15 17 10" />
    <line x1="12" y1="15" x2="12" y2="3" />
  </svg>
)

// Pencil icon for edit button
const PencilIcon = (
  <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor"
    strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
    <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
  </svg>
)

// One link shown as an individual card with QR code, title, details,
// click count, and optional edit/delete actions.
function LinkCard({ item, onDelete, onUpdateLink }) {
  const [qr, setQr] = useState(null)
  const [editing, setEditing] = useState(false)
  const [editTitle, setEditTitle] = useState('')
  const [editUrl, setEditUrl] = useState('')
  const [saving, setSaving] = useState(false)
  const fullShortUrl = window.location.origin + '/s/' + item.code

  useEffect(() => {
    let cancelled = false
    QRCode.toDataURL(fullShortUrl, { margin: 1, width: 128 })
      .then(url => { if (!cancelled) setQr(url) })
      .catch(() => {})
    return () => { cancelled = true }
  }, [item.code])

  const handleStartEdit = () => {
    setEditTitle(item.title || '')
    setEditUrl(item.url || '')
    setEditing(true)
  }

  const handleCancelEdit = () => {
    setEditing(false)
    setEditTitle('')
    setEditUrl('')
  }

  const handleSave = async () => {
    if (!onUpdateLink) return
    setSaving(true)
    try {
      await onUpdateLink(item.code, { title: editTitle, url: editUrl })
      setEditing(false)
    } catch {
      // error handled by caller
    } finally {
      setSaving(false)
    }
  }

  const handleKeyDown = (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); handleSave() }
    if (e.key === 'Escape') handleCancelEdit()
  }

  return (
    <div className="link-card">
      <div className="link-card-qr">
        {qr ? (
          <>
            <img src={qr} alt={`QR code for ${item.code}`} />
            <a
              className="qr-download"
              href={qr}
              download={`linker-${item.code}.png`}
              title="Download QR code"
            >
              {DownloadIcon}
            </a>
          </>
        ) : (
          <div className="qr-placeholder-sml" />
        )}
      </div>
      <div className="link-card-body">
        {editing ? (
          <div className="link-card-edit-fields">
            <input
              type="text"
              className="edit-input"
              value={editTitle}
              onChange={(e) => setEditTitle(e.target.value)}
              onKeyDown={handleKeyDown}
              autoFocus
              placeholder="Label"
            />
            <input
              type="text"
              inputMode="url"
              className="edit-input edit-input-url"
              value={editUrl}
              onChange={(e) => setEditUrl(e.target.value)}
              onKeyDown={handleKeyDown}
              placeholder="https://…"
            />
            <div className="link-card-edit-actions">
              <button className="btn btn-small" type="button" onClick={handleSave} disabled={saving}>
                {saving ? 'Saving…' : 'Save'}
              </button>
              <button className="btn btn-ghost btn-small" type="button" onClick={handleCancelEdit}>
                Cancel
              </button>
            </div>
          </div>
        ) : (
          <>
            <div className="link-card-title-row">
              <span className="link-card-title">
                {item.title || fullShortUrl}
              </span>
              {onUpdateLink && (
                <button className="btn-edit" type="button" onClick={handleStartEdit} title="Edit link">
                  {PencilIcon}
                </button>
              )}
            </div>
            <a className="link-card-short" href={fullShortUrl} target="_blank" rel="noreferrer">
              {fullShortUrl}
            </a>
            <span className="link-card-orig" title={item.url}>
              {item.url}
            </span>
            <div className="link-card-meta">
              <span className="link-card-clicks" title="Total redirects">
                {EyeIcon}
                {item.clicks} {item.clicks === 1 ? 'redirect' : 'redirects'}
              </span>
              {onDelete && (
                <button className="btn-delete" type="button" onClick={() => onDelete(item.code)} title="Delete link">
                  <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor"
                    strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                    <path d="M3 6h18" /><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6" /><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2" />
                  </svg>
                </button>
              )}
            </div>
          </>
        )}
      </div>
    </div>
  )
}

// The single page of the app:
//   topbar (sign-in / avatar) → form → POST /api/shorten → show short
//   URL + QR code → "my links" (when signed in) → session history

export default function App() {
  const [url, setUrl] = useState('')
  const [urlTitle, setUrlTitle] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState(null)
  const [history, setHistory] = useState([])
  const [copied, setCopied] = useState(false)

  // --- Auth state -----------------------------------------------------
  const [ssoEnabled, setSsoEnabled] = useState(false)
  const [user, setUser] = useState(null)
  const [myLinks, setMyLinks] = useState([])
  const [maxLinksPerUser, setMaxLinksPerUser] = useState(15)
  const [showSignInModal, setShowSignInModal] = useState(false)

  async function fetchMyLinks() {
    try {
      const res = await fetch('/api/me/links')
      if (!res.ok) return
      setMyLinks(await res.json())
    } catch {
      // API unreachable
    }
  }

  async function deleteLink(code) {
    try {
      const res = await fetch(`/api/links/${code}`, { method: 'DELETE' })
      if (res.ok) {
        setMyLinks(prev => prev.filter(l => l.code !== code))
      } else {
        const data = await res.json()
        setError(data.error || 'delete failed')
      }
    } catch {
      setError('Cannot reach the API')
    }
  }

  async function updateLink(code, { title, url }) {
    const res = await fetch(`/api/links/${code}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title, url }),
    })
    if (!res.ok) {
      const data = await res.json()
      setError(data.error || 'update failed')
      throw new Error('update failed')
    }
    // Update local state optimistically
    const patch = {}
    if (title !== undefined) patch.title = title
    if (url !== undefined) patch.url = url
    setMyLinks(prev => prev.map(l => l.code === code ? { ...l, ...patch } : l))
    setHistory(prev => prev.map(l => l.code === code ? { ...l, ...patch } : l))
    if (result && result.code === code) {
      setResult(prev => prev ? { ...prev, ...patch } : null)
    }
  }

  useEffect(() => {
    async function init() {
      try {
        const cfg = await fetch('/api/auth/config')
        if (cfg.ok) {
          const data = await cfg.json()
          setSsoEnabled(data.enabled)
          if (data.maxLinksPerUser) setMaxLinksPerUser(data.maxLinksPerUser)
        }
        const me = await fetch('/api/me')
        if (me.ok) {
          setUser(await me.json())
          fetchMyLinks()
        }
      } catch {
        // API offline
      }
    }
    init()
  }, [])

  useEffect(() => {
    if (ssoEnabled && !user) {
      setShowSignInModal(true)
    } else {
      setShowSignInModal(false)
    }
  }, [ssoEnabled, user])

  async function signOut() {
    try {
      await fetch('/api/logout', { method: 'POST' })
    } catch {
      // ignore
    }
    setUser(null)
    setMyLinks([])
  }

  async function handleSubmit(e) {
    e.preventDefault()
    if (busy) return

    setBusy(true)
    setError('')
    setCopied(false)

    try {
      const res = await fetch('/api/shorten', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url, title: urlTitle }),
      })
      const data = await res.json()

      if (!res.ok) {
        setError(data.error || `Request failed (${res.status})`)
        return
      }

      const fullShortUrl = window.location.origin + data.short_url
      const qr = await QRCode.toDataURL(fullShortUrl, { margin: 1, width: 316 })
      const item = { ...data, fullShortUrl, qr, when: new Date() }
      setResult(item)
      setHistory((h) => [item, ...h].slice(0, 10))
      setUrl('')
      setUrlTitle('')
      if (user) fetchMyLinks()
    } catch {
      setError('Cannot reach the API — is the backend running?')
    } finally {
      setBusy(false)
    }
  }

  async function copyShortUrl() {
    if (!result) return
    try {
      await navigator.clipboard.writeText(result.fullShortUrl)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      setError('Clipboard blocked by the browser — copy it manually.')
    }
  }

  // Total clicks across all my links
  const totalMyClicks = myLinks.reduce((sum, l) => sum + l.clicks, 0)

  return (
    <main className="shell">
      <div className="topbar">
        <span className="brand">
          {LinkIcon} Linker
        </span>
        {user ? (
          <div className="user-chip">
            {user.picture && (
              <img className="avatar" src={user.picture} alt="" referrerPolicy="no-referrer" />
            )}
            <span className="user-name">{user.name || user.email}</span>
            <button className="btn btn-ghost btn-small" type="button" onClick={signOut}>
              Sign out
            </button>
          </div>
        ) : (
          ssoEnabled && (
            <a className="btn btn-ghost btn-small google-btn" href="/auth/google/login">
              {GoogleIcon} Sign in with Google
            </a>
          )
        )}
      </div>

      <header className="hero">
        <h1>
          Make long links <span className="grad">beautifully short</span>
        </h1>
        <p className="subtitle">
          Paste any long URL and get a short, shareable link with a scannable
          QR code — all tracked to your account.
          {ssoEnabled && !user && ' Sign in to get started.'}
        </p>
      </header>

      <form className="card composer" onSubmit={handleSubmit}>
        <div className="composer-url">
          {LinkIcon}
          <input
            type="text"
            inputMode="url"
            aria-label="Long URL to shorten"
            placeholder={user ? 'Paste your long URL here…' : 'Sign in to shorten links →'}
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            required
          />
          <button className="btn" type="submit" disabled={busy || !url.trim()}>
            {busy && <span className="spinner" aria-hidden="true" />}
            {busy ? 'Shortening…' : 'Shorten'}
          </button>
        </div>
        <div className="composer-title">
          {TitleIcon}
          <input
            type="text"
            aria-label="Link title (optional)"
            placeholder="Add a label so you can tell your links apart…"
            value={urlTitle}
            onChange={(e) => setUrlTitle(e.target.value)}
          />
        </div>
      </form>

      {error && error === 'sign in required' ? (
        <div className="card signin-prompt" role="alert">
          <p>🔐 Sign in to start shortening links.</p>
          <a className="btn google-btn" href="/auth/google/login">
            {GoogleIcon} Sign in with Google
          </a>
        </div>
      ) : error && (
        <div className="banner" role="alert">
          {error}
        </div>
      )}

      {result && (
        <section className="card result" aria-live="polite">
          <div className="result-main">
            <span className="label">Your short link</span>
            {result.title && (
              <span className="result-title">{result.title}</span>
            )}
            <a className="short-link" href={result.fullShortUrl} target="_blank" rel="noreferrer">
              {result.fullShortUrl}
            </a>
            <p className="original" title={result.url}>
              {result.url}
            </p>
            <div className="actions">
              <button className="btn btn-small" type="button" onClick={copyShortUrl}>
                {copied ? '✓ Copied' : 'Copy link'}
              </button>
              <a className="btn btn-ghost btn-small" href={result.fullShortUrl} target="_blank" rel="noreferrer">
                Open ↗
              </a>
            </div>
          </div>
          <div className="qr-frame">
            <img src={result.qr} alt={`QR code for ${result.fullShortUrl}`} />
            <span className="qr-caption">Scan me</span>
            <a
              className="qr-download qr-download-result"
              href={result.qr}
              download={`linker-${result.code}-qr.png`}
              title="Download QR code"
            >
              {DownloadIcon} Download QR
            </a>
          </div>
        </section>
      )}

      {user && myLinks.length > 0 && (
        <section className="card history my-links">
          <div className="history-head">
            <h2>My links</h2>
            <span className="count">{myLinks.length} / {maxLinksPerUser}</span>
            {totalMyClicks > 0 && (
              <span className="click-total">
                {EyeIcon} {totalMyClicks} {totalMyClicks === 1 ? 'redirect' : 'redirects'}
              </span>
            )}
          </div>
          <div className="link-card-grid">
            {myLinks.map((item) => (
              <LinkCard key={item.code} item={item} onDelete={deleteLink} onUpdateLink={updateLink} />
            ))}
          </div>
        </section>
      )}

      {history.length > 0 && (
        <section className="card history">
          <div className="history-head">
            <h2>This session</h2>
            <span className="count">{history.length}</span>
          </div>
          <div className="link-card-grid">
            {history.map((item) => (
              <LinkCard key={item.code + item.when.getTime()} item={item} />
            ))}
          </div>
        </section>
      )}

      {showSignInModal && (
        <div className="modal-overlay">
          <div className="modal-card">
            <h1 className="modal-title">
              URL Shortener &amp; <span className="grad">QR Generator</span>
            </h1>
            <p className="modal-desc">
              Turn long links into short, shareable URLs and scannable QR codes
              — all tracked to your account.
            </p>

            <div className="modal-divider" />

            <div className="modal-signin">
              <span className="modal-lock">🔐</span>
              <p className="modal-warning">
                <strong>Sign in required</strong> — use your Google account to
                get started. Every link is tracked to its owner.
              </p>
              <a className="btn google-btn" href="/auth/google/login">
                {GoogleIcon} Sign in with Google
              </a>
            </div>
          </div>
        </div>
      )}
    </main>
  )
}