import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Link, useLocation, useNavigationType, useSearchParams } from 'react-router-dom'
import { Activity, ArrowDownAZ, ArrowLeft, ArrowUpDown, ChevronRight, Clapperboard, Folder, FolderOpen, Grid2X2, Image, LayoutList, LogOut, MoreVertical, Play, Search, UserRound, Users, X } from 'lucide-react'
import { request, time } from '../api/client'
import type { Library, Media, Session } from '../api/client'
import Dialog from '../components/Dialog'
import LockSettings from '../components/LockSettings'

type LibraryMode = 'player' | 'explorer'
const MODE_KEY = 'mediaex:library-mode'
const scrollPositions = new Map<string, number>()

function readMode(): LibraryMode {
  try { return localStorage.getItem(MODE_KEY) === 'explorer' ? 'explorer' : 'player' } catch { return 'player' }
}

function Thumbnail({ item }: { item: Media }) {
  return <div className="thumbnail"><img src={item.image} alt="" loading="lazy" />{item.kind === 'video' && <span className="duration">{time(item.duration)}</span>}</div>
}

export default function LibraryPage({ session, onLogout, revision, onRetry, lockBusy, lockError, onEnableLock, onDisableLock, onLock }: { session: Session; onLogout: () => void; revision: number; onRetry: () => void; lockBusy: boolean; lockError: string; onEnableLock: (password: string, existing: boolean) => void; onDisableLock: (password: string) => void; onLock: () => void }) {
  const [params, setParams] = useSearchParams()
  const [mode, setMode] = useState<LibraryMode>(readMode)
  const explorer = mode === 'explorer'
  const folderId = explorer ? params.get('folder') || '' : ''
  const query = params.get('q') || ''
  const [data, setData] = useState<Library | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [reload, setReload] = useState(0)
  const requestKey = `${mode}:${folderId}:${query}:${reload}:${revision}`
  const [resolvedKey, setResolvedKey] = useState('')
  const [grid, setGrid] = useState(false)
  const [ascending, setAscending] = useState(false)
  const [modal, setModal] = useState<'account' | null>(null)
  const [detail, setDetail] = useState<Media | null>(null)
  const [busy, setBusy] = useState(false)
  const location = useLocation()
  const navigationType = useNavigationType()
  const scrollKey = `${session?.id}:${mode}:${location.key}`
  const restoredScrollKey = useRef<string | null>(null)
  const ready = !loading && resolvedKey === requestKey && !error
  useLayoutEffect(() => {
    restoredScrollKey.current = null
    const saveScroll = () => {
      if (restoredScrollKey.current === scrollKey) scrollPositions.set(scrollKey, window.scrollY)
    }
    window.addEventListener('scroll', saveScroll, { passive: true })
    return () => window.removeEventListener('scroll', saveScroll)
  }, [scrollKey])
  useLayoutEffect(() => {
    if (!ready || restoredScrollKey.current === scrollKey) return
    const top = navigationType === 'POP' ? scrollPositions.get(scrollKey) ?? 0 : 0
    window.scrollTo({ top, left: 0, behavior: 'instant' })
    restoredScrollKey.current = scrollKey
    scrollPositions.set(scrollKey, window.scrollY)
  }, [ready, scrollKey, navigationType])
  useEffect(() => {
    let active = true
    request<Library>(`/library?scope=${explorer ? 'folder' : 'all'}&folder=${encodeURIComponent(folderId)}&q=${encodeURIComponent(query)}`).then((value) => { if (active) { setData(value); setError('') } }).catch((error: Error) => { if (active) setError(error.message) }).finally(() => { if (active) { setLoading(false); setResolvedKey(requestKey) } })
    return () => { active = false }
  }, [explorer, folderId, query, reload, requestKey])
  function update(next: { folder?: string; q?: string }) {
    setLoading(true); setError('')
    setParams(Object.fromEntries(Object.entries(next).filter(([, value]) => value)))
  }
  function changeMode(next: LibraryMode) {
    try { localStorage.setItem(MODE_KEY, next) } catch { /* The current session still keeps the selection. */ }
    setMode(next)
    setDetail(null)
    setGrid(false)
    update({})
    window.scrollTo({ top: 0, left: 0 })
  }
  async function logout() {
    setBusy(true)
    try { await request('/logout', { method: 'POST' }); onLogout() } catch (error) { setError((error as Error).message); setModal(null) } finally { setBusy(false) }
  }
  const items = data?.items || []
  const files = ascending ? [...items].sort((a, b) => a.name.localeCompare(b.name)) : items
  const home = !folderId && !query
  const showFolders = explorer && !query
  const rootLabel = explorer ? 'Library root' : 'All media'
  const trail = data?.breadcrumbs?.length ? data.breadcrumbs : data?.folder ? [data.folder] : []
  return <div className={`app-shell ${explorer ? 'explorer-library' : 'feed-library'}`}>
    <aside className="sidebar">
      <Link className="brand" to="/library"><span className="brand-icon"><FolderOpen size={22} /></span><span>Media Explorer<span className="brand-subtitle">A home for your collection</span></span></Link>
      <span className="nav-label">WORKSPACE</span>
      <nav aria-label="Main navigation">
        <Link className={`nav-item ${home ? 'active' : ''}`} to="/library"><FolderOpen size={19} />{rootLabel}</Link>
      </nav>
      {session?.role === 'admin' && <nav aria-label="Administration"><Link className="nav-item" to="/admin/users"><Users size={18} />Manage accounts</Link><Link className="nav-item" to="/admin/issues"><Activity size={18} />Library health</Link></nav>}
      {explorer && !!data?.navigation?.length && <>
        <span className="nav-label folder-nav-label">YOUR FOLDERS</span>
        <nav aria-label="Folders">{data.navigation.map((folder) => <Link className={`nav-item ${folderId === folder.id ? 'active' : ''}`} key={folder.id} to={`/library?folder=${folder.id}`}><Folder size={18} /><span>{folder.name}</span></Link>)}</nav>
      </>}
    </aside>
    <div className="main-shell">
      <header className="app-header"><Link className="mobile-brand" to="/library"><FolderOpen size={23} /><span>Media Explorer</span></Link><div className="header-actions"><button className="avatar" aria-label="Open account" onClick={() => setModal('account')}><UserRound size={20} /></button></div></header>
      <main className="library-main">
        <h1 className="sr-only">{query ? 'Search your collection' : data?.folder?.name || 'Media library'}</h1>
        <div className="search-row"><div className="search-field"><Search size={19} /><input aria-label="Search your media" placeholder="Search your media…" value={query} onChange={(event) => update({ folder: folderId, q: event.target.value })} />{query ? <button className="icon-button" aria-label="Clear search" onClick={() => update({ folder: folderId })}><X size={17} /></button> : <span className="search-hint">Find a little something</span>}</div></div>
        {folderId && <nav className="breadcrumbs" aria-label="Breadcrumb"><Link to="/library"><ArrowLeft size={15} />Library root</Link>{trail.map((crumb, index) => <span key={crumb.id} className="crumb"><ChevronRight size={14} />{index === trail.length - 1 ? <span>{crumb.name}</span> : <Link to={`/library?folder=${crumb.id}`}>{crumb.name}</Link>}</span>)}</nav>}
        {loading || resolvedKey !== requestKey ? <div className="library-skeleton" role="status" aria-label="Loading media">{showFolders && <><div className="skeleton skeleton-heading" /><div className="folder-grid">{[1, 2, 3, 4, 5, 6].map((id) => <div key={id} className="skeleton skeleton-row" />)}</div></>}<div className="skeleton skeleton-heading" />{[1, 2, 3].map((id) => <div key={id} className="skeleton skeleton-row" />)}<span className="sr-only">Loading your media</span></div> : error ? <div className="error-state" role="alert"><Clapperboard size={32} /><h2>Couldn't load your collection</h2><p>{error}</p><button className="button primary" onClick={() => { setLoading(true); setError(''); onRetry(); setReload(reload + 1) }}>Try again</button></div> : <>
          {showFolders && !!data?.folders.length && <section className="folders-section"><div className="section-heading"><h2>Folders</h2></div><div className="folder-grid">{data.folders.map((folder) => <Link className="folder-card" key={folder.id} to={`/library?folder=${folder.id}`}><span className="folder-icon"><Folder size={32} fill="currentColor" strokeWidth={1.3} /></span><div className="folder-card-body"><h3>{folder.name}</h3><p>{folder.count} {folder.count === 1 ? 'item' : 'items'}</p></div><ChevronRight size={18} className="folder-chevron" /></Link>)}</div></section>}
          <section className="files-section"><div className="section-heading"><h2>{query ? 'Search results' : explorer ? 'Files' : 'All media'}</h2><div className="file-actions"><button className={`sort-button ${ascending ? 'selected' : ''}`} onClick={() => setAscending(!ascending)} aria-label="Sort files by name" aria-pressed={ascending}>{ascending ? <ArrowDownAZ size={15} /> : <ArrowUpDown size={15} />}<span>{ascending ? 'Name' : 'Default order'}</span></button>{explorer && <div className="view-toggle"><button className={!grid ? 'selected' : ''} aria-label="List view" aria-pressed={!grid} onClick={() => setGrid(false)}><LayoutList size={17} /></button><button className={grid ? 'selected' : ''} aria-label="Grid view" aria-pressed={grid} onClick={() => setGrid(true)}><Grid2X2 size={17} /></button></div>}</div></div>
            {files.length ? <div className={`file-list ${!explorer ? 'file-grid media-feed' : grid ? 'file-grid' : ''}`}><div className="file-table-head"><span>NAME</span><span>DETAILS</span><span>TYPE</span></div>{files.map((item) => <article key={item.id} className="file-row"><Link to={`/media/${item.id}`} className="file-main"><Thumbnail item={item} /><span className="file-type-icon">{item.kind === 'video' ? <Clapperboard size={19} /> : <Image size={19} />}</span><div className="file-name"><h3>{item.name}</h3><p>{!explorer && <>{item.kind === 'video' ? 'Video' : 'Photo'}<span> · </span></>}{item.resolution}<span> · </span>{item.size}<span> · </span>{item.format}</p></div></Link><span className="file-details">{item.resolution}<span>{item.size}</span></span><span className={`file-format ${item.kind}`}>{item.format}</span><button className="icon-button file-menu" aria-label={`Details for ${item.name}`} onClick={() => setDetail(item)}><MoreVertical size={20} /></button></article>)}</div> : <div className="empty-state"><FolderOpen size={39} /><h3>{query ? 'No matching moments' : 'A little room for more'}</h3><p>{query ? 'Try another name or clear your search.' : showFolders && !!data?.folders.length ? 'Open a folder above to browse its contents.' : 'There are no files in this view yet.'}</p>{query && <button className="button subtle" onClick={() => update({ folder: folderId })}>Clear search</button>}</div>}
          </section>
        </>}
      </main>
    </div>
    {modal === 'account' && <Dialog title="Your account" onClose={() => setModal(null)}><div className="account-card"><span className="avatar"><UserRound size={23} /></span><div><strong>{session?.name}</strong></div></div>{session && <LockSettings session={session} busy={lockBusy} error={lockError} onEnable={onEnableLock} onDisable={onDisableLock} onLock={onLock} />}<button className="button subtle" disabled={busy} onClick={logout}><LogOut size={17} />{busy ? 'Signing out…' : 'Sign out'}</button><div className="library-mode-setting"><label htmlFor="library-mode">Mode tampilan</label><select id="library-mode" value={mode} onChange={(event) => changeMode(event.target.value as LibraryMode)}><option value="player">Media Player</option><option value="explorer">Explorer</option></select></div>{session?.role === 'admin' && <Link className="button subtle account-admin-link" to="/admin/users"><Users size={17} />Manage accounts</Link>}</Dialog>}
    {detail && <Dialog title="File details" onClose={() => setDetail(null)}><img className="detail-image" src={detail.image} alt="" /><h3 className="detail-name">{detail.name}</h3><dl className="detail-list"><div><dt>Format</dt><dd>{detail.format}</dd></div><div><dt>Resolution</dt><dd>{detail.resolution}</dd></div><div><dt>Folder</dt><dd>{detail.folderId}</dd></div>{detail.kind === 'video' && <div><dt>Duration</dt><dd>{time(detail.duration)}</dd></div>}</dl><Link className="button primary" to={`/media/${detail.id}`}>{detail.kind === 'video' ? <Play size={16} /> : <Image size={16} />}{detail.kind === 'video' ? 'Open player' : 'View image'}</Link></Dialog>}
  </div>
}
