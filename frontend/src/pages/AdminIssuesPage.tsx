import { useCallback, useEffect, useMemo, useState } from 'react'
import type { FormEvent } from 'react'
import { Activity, AlertTriangle, Ban, ChevronLeft, ChevronRight, FolderOpen, ListVideo, RotateCcw, Search, Users } from 'lucide-react'
import { Link } from 'react-router-dom'
import { request, time } from '../api/client'
import type { IssueItem, IssuePage, IssueStage, Session } from '../api/client'
import Dialog from '../components/Dialog'

type Tab = 'failed' | 'queue' | 'skipped'
type SkipTarget = { ids: string[]; label: string }
const PAGE_SIZE = 50
const STAGE_LABEL: Record<IssueStage, string> = { probe: 'Metadata', sprite: 'Preview', conversion: 'Conversion', queue: 'Conversion', skipped: 'Skipped' }
const STATUS_LABEL: Record<string, string> = { pending: 'Queued', processing: 'Converting now', waiting_source: 'Waiting for file' }

function bytes(size: number) {
  if (!Number.isFinite(size) || size <= 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = size
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++ }
  return `${value >= 10 || unit === 0 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`
}
function stamp(seconds: number | null | undefined) {
  if (!seconds) return '—'
  return new Date(seconds * 1000).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}
function future(seconds: number) {
  const diff = seconds * 1000 - Date.now()
  if (diff <= 0) return 'Due now'
  const minutes = Math.round(diff / 60000)
  if (minutes < 1) return 'In under a minute'
  if (minutes < 60) return `In ${minutes}m`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `In ${hours}h`
  return `In ${Math.round(hours / 24)}d`
}
function nextLabel(item: IssueItem) {
  if (item.stage === 'conversion') return 'Stopped'
  if (!item.nextRetryAt) return 'Stopped'
  return future(item.nextRetryAt)
}
function attempts(item: IssueItem) {
  if (item.maxAttempts === null) return `${item.attempt} · automatic`
  return `${item.attempt} / ${item.maxAttempts}`
}
function detailLine(item: IssueItem) {
  if (item.stage === 'queue') return STATUS_LABEL[item.status || ''] || item.status || 'Queued'
  if (item.stage === 'skipped') return item.reason || 'No note'
  return item.reason || 'No error detail'
}

export default function AdminIssuesPage({ session, onLogout }: { session: Session; onLogout: () => void }) {
  const [tab, setTab] = useState<Tab>('failed')
  const [query, setQuery] = useState('')
  const [search, setSearch] = useState('')
  const [offset, setOffset] = useState(0)
  const [page, setPage] = useState<IssuePage | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [detail, setDetail] = useState<IssueItem | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [skipTarget, setSkipTarget] = useState<SkipTarget | null>(null)
  const [skipNote, setSkipNote] = useState('')

  useEffect(() => {
    const timer = window.setTimeout(() => { setSearch(query.trim()); setOffset(0); setSelected(new Set()) }, 250)
    return () => window.clearTimeout(timer)
  }, [query])

  const load = useCallback(async (quiet = false) => {
    if (!quiet) setError('')
    try {
      const data = await request<IssuePage>(`/admin/issues?tab=${tab}&q=${encodeURIComponent(search)}&limit=${PAGE_SIZE}&offset=${offset}`)
      setPage(data)
      setError('')
    } catch (cause) {
      if (!quiet) setError((cause as Error).message)
    }
  }, [tab, search, offset])

  useEffect(() => {
    let active = true
    request<IssuePage>(`/admin/issues?tab=${tab}&q=${encodeURIComponent(search)}&limit=${PAGE_SIZE}&offset=${offset}`)
      .then(data => { if (active) { setPage(data); setError('') } })
      .catch((cause: Error) => { if (active) setError(cause.message) })
    return () => { active = false }
  }, [tab, search, offset])

  useEffect(() => {
    const timer = window.setInterval(() => { if (document.visibilityState === 'visible') void load(true) }, 10000)
    return () => window.clearInterval(timer)
  }, [load])

  useEffect(() => {
    if (!notice) return
    const timer = window.setTimeout(() => setNotice(''), 3600)
    return () => window.clearTimeout(timer)
  }, [notice])

  async function logout() {
    setBusy(true)
    try { await request('/logout', { method: 'POST' }); onLogout() }
    catch (cause) { setError((cause as Error).message) }
    finally { setBusy(false) }
  }

  async function act(path: string, method: string, body: unknown, message: string) {
    setBusy(true)
    setError('')
    try {
      await request(path, { method, body: body === undefined ? undefined : JSON.stringify(body) })
      setNotice(message)
      setDetail(null)
      setSkipTarget(null)
      setSkipNote('')
      await load(true)
    } catch (cause) {
      setError((cause as Error).message)
    } finally {
      setBusy(false)
    }
  }

  function submitSkip(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!skipTarget) return
    if (skipTarget.ids.length === 1) void act(`/admin/issues/${encodeURIComponent(skipTarget.ids[0])}/skip`, 'POST', { reason: skipNote }, 'Item skipped.')
    else void act('/admin/issues/skip', 'POST', { ids: skipTarget.ids, reason: skipNote }, `${skipTarget.ids.length} items skipped.`)
  }

  function toggle(id: string) {
    setSelected(current => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const items = page?.items || []
  const counts = page?.counts || { probe: 0, sprite: 0, conversions: 0, queue: 0, skipped: 0, processing: 0 }
  const failed = counts.probe + counts.sprite + counts.conversions
  const first = page && page.total ? page.offset + 1 : 0
  const last = page ? Math.min(page.offset + page.items.length, page.total) : 0
  const allSelected = items.length > 0 && items.every(item => selected.has(item.id))
  const tabItems = useMemo(() => [{ key: 'failed' as Tab, label: 'Failed', value: failed }, { key: 'queue' as Tab, label: 'Conversion queue', value: counts.queue }, { key: 'skipped' as Tab, label: 'Skipped', value: counts.skipped }], [failed, counts.queue, counts.skipped])

  function choose(next: Tab) {
    setTab(next)
    setOffset(0)
    setQuery('')
    setSearch('')
    setSelected(new Set())
  }

  return <div className="app-shell">
    <aside className="sidebar">
      <Link className="brand" to="/library"><span className="brand-icon"><FolderOpen size={22} /></span><span>Media Explorer<span className="brand-subtitle">A home for your collection</span></span></Link>
      <span className="nav-label">WORKSPACE</span>
      <nav aria-label="Main navigation"><Link className="nav-item" to="/library"><ChevronLeft size={19} />Back to library</Link></nav>
      <span className="nav-label folder-nav-label">ADMINISTRATION</span>
      <nav aria-label="Administration">
        <Link className="nav-item" to="/admin/users"><Users size={18} />Manage accounts</Link>
        <span className="nav-item active" aria-current="page"><Activity size={18} />Library health</span>
      </nav>
    </aside>
    <div className="main-shell">
      <header className="app-header">
        <Link className="mobile-brand" to="/library"><FolderOpen size={23} /><span>Media Explorer</span></Link>
        <div className="header-actions"><span className="header-account">{session?.name}</span><button className="button subtle" disabled={busy} onClick={logout}>Sign out</button></div>
      </header>
      <main className="library-main">
        <div className="section-heading issues-heading">
          <div>
            <h1>Library health</h1>
            <p className="section-note">See what needs attention and stop work on files you do not want processed.</p>
          </div>
          <button className="button subtle" onClick={() => void load()}>Refresh</button>
        </div>
        <p className="issue-help">
          <strong>Metadata</strong> means the file could not be read. <strong>Preview</strong> means its hover sheet failed. <strong>Conversion</strong> is the optional browser-friendly copy.
          Skipping a file leaves it untouched until you restore it.
        </p>

        <section className="issue-cards" aria-label="Processing summary">
          <button className={`issue-card ${counts.processing > 0 ? 'live' : ''}`} onClick={() => choose('queue')}>
            <span className="issue-card-label"><Activity size={15} />Running now</span>
            <strong>{counts.processing}</strong>
            <span className="issue-card-note">{counts.processing === 1 ? 'conversion in progress' : 'conversions in progress'}</span>
          </button>
          <button className="issue-card" onClick={() => choose('queue')}>
            <span className="issue-card-label"><ListVideo size={15} />Conversion queue</span>
            <strong>{counts.queue}</strong>
            <span className="issue-card-note">waiting or running</span>
          </button>
          <button className={`issue-card ${failed ? 'attention' : ''}`} onClick={() => choose('failed')}>
            <span className="issue-card-label"><AlertTriangle size={15} />Failed</span>
            <strong>{failed}</strong>
            <span className="issue-card-note">{counts.probe} metadata · {counts.sprite} preview · {counts.conversions} conversion</span>
          </button>
          <button className="issue-card" onClick={() => choose('skipped')}>
            <span className="issue-card-label"><Ban size={15} />Skipped</span>
            <strong>{counts.skipped}</strong>
            <span className="issue-card-note">left untouched by scans</span>
          </button>
        </section>

        {notice && <p className="toast-inline" role="status">{notice}</p>}
        {error && <div className="error-state" role="alert"><h2>Couldn’t load library health</h2><p>{error}</p><button className="button primary" onClick={() => void load()}>Try again</button></div>}

        <div className="issue-toolbar">
          <div className="issue-tabs" role="tablist" aria-label="Issue category">
            {tabItems.map(entry => <button key={entry.key} role="tab" aria-selected={tab === entry.key} className={`issue-tab ${tab === entry.key ? 'active' : ''}`} onClick={() => choose(entry.key)}>
              {entry.label}<span className="issue-tab-count">{entry.value}</span>
            </button>)}
          </div>
          <label className="issue-search">
            <Search size={16} />
            <input value={query} onChange={event => setQuery(event.target.value)} placeholder="Search name or path" aria-label="Search files" />
          </label>
        </div>

        {selected.size > 0 && <div className="issue-selection" role="status">
          <span>{selected.size} selected</span>
          <button className="button subtle compact" disabled={busy} onClick={() => { setSkipNote(''); setSkipTarget({ ids: [...selected], label: `${selected.size} selected items` }) }}><Ban size={15} />Skip selected</button>
          <button className="button subtle compact" onClick={() => setSelected(new Set())}>Clear</button>
        </div>}

        {!page && !error && <div className="loading-state" role="status"><span className="spinner" />Loading library health…</div>}
        {page && !items.length && <div className="empty-state">
          {tab === 'failed' ? <AlertTriangle size={38} /> : tab === 'queue' ? <ListVideo size={38} /> : <Ban size={38} />}
          <h3>{tab === 'failed' ? 'Nothing has failed' : tab === 'queue' ? 'The conversion queue is empty' : 'No skipped files'}</h3>
          <p>{tab === 'failed' ? 'Every file currently in the library has been processed successfully.' : tab === 'queue' ? 'Nothing is waiting to be converted right now.' : 'Files you skip will appear here so you can restore them later.'}</p>
        </div>}

        {!!items.length && <section className="issue-list" aria-label="Issue list">
          <div className="issue-head">
            <input type="checkbox" aria-label="Select all" checked={allSelected} onChange={() => setSelected(allSelected ? new Set() : new Set(items.map(item => item.id)))} />
            <span>FILE</span><span>{tab === 'queue' ? 'STATUS / REASON' : tab === 'skipped' ? 'NOTE' : 'PROBLEM'}</span><span>{tab === 'skipped' ? 'SKIPPED' : tab === 'queue' ? 'STARTED' : 'ATTEMPTS'}</span><span>{tab === 'skipped' ? 'BY' : tab === 'queue' ? 'STATE' : 'NEXT RETRY'}</span><span className="sr-only">Actions</span>
          </div>
          {items.map(item => <article className="issue-row" key={`${item.stage}-${item.id}`}>
            <input type="checkbox" aria-label={`Select ${item.name}`} checked={selected.has(item.id)} onChange={() => toggle(item.id)} />
            <button className="issue-file" onClick={() => setDetail(item)} title={item.path}>
              <strong>{item.name}</strong>
              <span>{item.path}</span>
            </button>
            <div className="issue-detail">
              <span className={`issue-stage ${tab === 'failed' ? 'bad' : ''}`}>{item.stage === 'queue' ? STATUS_LABEL[item.status || ''] || 'Queued' : STAGE_LABEL[item.stage]}</span>
              <span className="issue-reason" title={detailLine(item)}>{detailLine(item)}</span>
            </div>
            <span className="issue-meta">{tab === 'skipped' ? stamp(item.updatedAt) : tab === 'queue' ? (item.updatedAt ? `since ${stamp(item.updatedAt)}` : '—') : attempts(item)}</span>
            <span className="issue-meta">{tab === 'skipped' ? item.skippedBy || '—' : tab === 'queue' ? (item.status === 'processing' ? 'In progress' : 'Waiting') : nextLabel(item)}</span>
            <div className="issue-actions">
              {tab === 'skipped'
                ? <button className="button subtle compact" disabled={busy} onClick={() => void act(`/admin/issues/${encodeURIComponent(item.id)}/skip`, 'DELETE', undefined, 'Item restored to the library.')}><RotateCcw size={15} />Restore</button>
                : <>
                  {tab === 'failed' && <button className="button subtle compact" disabled={busy} onClick={() => void act(`/admin/issues/${encodeURIComponent(item.id)}/retry`, 'POST', {}, 'Item queued for another try.')}><RotateCcw size={15} />Try again</button>}
                  <button className="button subtle compact" disabled={busy} onClick={() => { setSkipNote(''); setSkipTarget({ ids: [item.id], label: item.name }) }}><Ban size={15} />Skip</button>
                </>}
            </div>
          </article>)}
        </section>}

        {page && page.total > 0 && <div className="issue-pager">
          <span>{first}–{last} of {page.total}</span>
          <button className="button subtle compact" disabled={offset === 0 || busy} onClick={() => { setSelected(new Set()); setOffset(Math.max(0, offset - PAGE_SIZE)) }}><ChevronLeft size={16} />Previous</button>
          <button className="button subtle compact" disabled={last >= page.total || busy} onClick={() => { setSelected(new Set()); setOffset(offset + PAGE_SIZE) }}>Next<ChevronRight size={16} /></button>
        </div>}
      </main>
    </div>

    {detail && <Dialog title={detail.name} onClose={() => setDetail(null)} className="issue-dialog">
      <dl className="issue-facts">
        <div><dt>Path</dt><dd className="issue-path">{detail.path}</dd></div>
        <div><dt>Problem</dt><dd>{STAGE_LABEL[detail.stage]}{detail.status ? ` · ${STATUS_LABEL[detail.status] || detail.status}` : ''}</dd></div>
        <div><dt>Type</dt><dd>{detail.format || detail.kind} · {bytes(detail.size)} · {detail.width > 0 ? `${detail.width}×${detail.height}` : 'unknown size'}{detail.duration > 0 ? ` · ${time(detail.duration)}` : ''}</dd></div>
        <div><dt>Attempts</dt><dd>{attempts(detail)}</dd></div>
        <div><dt>Next retry</dt><dd>{detail.stage === 'queue' ? '—' : nextLabel(detail)}</dd></div>
        <div><dt>{detail.stage === 'skipped' ? 'Skipped' : 'Last update'}</dt><dd>{stamp(detail.updatedAt)}{detail.skippedBy ? ` by ${detail.skippedBy}` : ''}</dd></div>
        <div><dt>File modified</dt><dd>{stamp(detail.modifiedAt)}</dd></div>
        {detail.strategy && detail.strategy !== 'none' && <div><dt>Playback plan</dt><dd>{detail.strategy.replace('_', ' ')}</dd></div>}
      </dl>
      <p className="issue-error-label">Detail from the media tool</p>
      <pre className="issue-error">{detail.reason || 'No detail was recorded.'}</pre>
      <div className="issue-dialog-actions">
        {detail.stage === 'skipped'
          ? <button className="button primary" disabled={busy} onClick={() => void act(`/admin/issues/${encodeURIComponent(detail.id)}/skip`, 'DELETE', undefined, 'Item restored to the library.')}><RotateCcw size={16} />Restore</button>
          : <>
            {detail.stage !== 'queue' && <button className="button subtle" disabled={busy} onClick={() => void act(`/admin/issues/${encodeURIComponent(detail.id)}/retry`, 'POST', {}, 'Item queued for another try.')}><RotateCcw size={16} />Try again</button>}
            <button className="button primary" disabled={busy} onClick={() => { setSkipNote(''); setDetail(null); setSkipTarget({ ids: [detail.id], label: detail.name }) }}><Ban size={16} />Skip this file</button>
          </>}
      </div>
    </Dialog>}

    {skipTarget && <Dialog title="Skip processing" onClose={() => setSkipTarget(null)}>
      <form onSubmit={submitSkip}>
        <p className="muted">{skipTarget.ids.length === 1 ? 'This file will stay in the catalogue but will not be probed, previewed, or converted until you restore it. Its media file is never changed.' : `${skipTarget.ids.length} files will stay in the catalogue but will not be probed, previewed, or converted until you restore them. Their media files are never changed.`}</p>
        <label className="field-label" htmlFor="skip-note">Note (optional)</label>
        <div className="dialog-input"><input id="skip-note" value={skipNote} maxLength={500} onChange={event => setSkipNote(event.target.value)} placeholder="e.g. broken download, keep for now" /></div>
        <div className="issue-dialog-actions">
          <button className="button primary" type="submit" disabled={busy}><Ban size={16} />{busy ? 'Skipping…' : 'Skip'}</button>
          <button className="button subtle" type="button" disabled={busy} onClick={() => setSkipTarget(null)}>Cancel</button>
        </div>
      </form>
    </Dialog>}
  </div>
}
