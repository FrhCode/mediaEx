import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { Activity, ChevronLeft, FolderOpen, KeyRound, Plus, ShieldCheck, Trash2, UserRound, UserX } from 'lucide-react'
import { Link } from 'react-router-dom'
import { request } from '../api/client'
import type { Account, Session } from '../api/client'
import Dialog from '../components/Dialog'

type Draft = { mode: 'create' } | { mode: 'password'; account: Account }
type Confirm = { account: Account; action: 'disable' | 'enable' | 'delete' }

export default function AdminUsersPage({ session, onLogout }: { session: Session; onLogout: () => void }) {
  const [accounts, setAccounts] = useState<Account[] | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [dialogError, setDialogError] = useState('')
  const [draft, setDraft] = useState<Draft | null>(null)
  const [confirm, setConfirm] = useState<Confirm | null>(null)
  async function logout() {
    setBusy(true)
    try { await request('/logout', { method: 'POST' }); onLogout() }
    catch (cause) { setError((cause as Error).message) }
    finally { setBusy(false) }
  }

  const load = useCallback(async () => {
    try {
      const data = await request<{ users: Account[] }>('/admin/users')
      setAccounts(data.users)
      setError('')
    } catch (cause) {
      setError((cause as Error).message)
    }
  }, [])

  useEffect(() => {
    let active = true
    request<{ users: Account[] }>('/admin/users')
      .then((data) => { if (active) { setAccounts(data.users); setError('') } })
      .catch((cause: Error) => { if (active) setError(cause.message) })
    return () => { active = false }
  }, [])
  useEffect(() => {
    if (!notice) return
    const timer = window.setTimeout(() => setNotice(''), 3200)
    return () => window.clearTimeout(timer)
  }, [notice])

  async function submitDraft(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!draft) return
    const fields = new FormData(event.currentTarget)
    setBusy(true)
    setDialogError('')
    try {
      if (draft.mode === 'create') {
        await request('/admin/users', {
          method: 'POST',
          body: JSON.stringify({
            username: String(fields.get('username') || '').trim(),
            name: String(fields.get('name') || '').trim(),
            password: fields.get('password'),
            role: fields.get('role'),
          }),
        })
        setNotice('Account created.')
      } else {
        await request(`/admin/users/${encodeURIComponent(draft.account.id)}`, { method: 'PATCH', body: JSON.stringify({ password: fields.get('password') }) })
        setNotice('Password updated.')
      }
      setDraft(null)
      await load()
    } catch (cause) {
      setDialogError((cause as Error).message)
    } finally {
      setBusy(false)
    }
  }

  async function apply(action: 'disable' | 'enable' | 'delete', account: Account) {
    setBusy(true)
    setError('')
    try {
      if (action === 'delete') await request(`/admin/users/${encodeURIComponent(account.id)}`, { method: 'DELETE' })
      else await request(`/admin/users/${encodeURIComponent(account.id)}`, { method: 'PATCH', body: JSON.stringify({ active: action === 'enable' }) })
      setNotice(account.username + (action === 'delete' ? ' was removed.' : action === 'enable' ? ' is active again.' : ' can no longer sign in.'))
      setConfirm(null)
      await load()
    } catch (cause) {
      setError((cause as Error).message)
      setConfirm(null)
    } finally {
      setBusy(false)
    }
  }

  const list = accounts || []
  const activeAdmins = list.filter((account) => account.active && account.role === 'admin').length

  return <div className="app-shell">
    <aside className="sidebar">
      <Link className="brand" to="/library"><span className="brand-icon"><FolderOpen size={22} /></span><span>Media Explorer<span className="brand-subtitle">A home for your collection</span></span></Link>
      <span className="nav-label">WORKSPACE</span>
      <nav aria-label="Main navigation"><Link className="nav-item" to="/library"><ChevronLeft size={19} />Back to library</Link></nav>
      <span className="nav-label folder-nav-label">ADMINISTRATION</span>
      <nav aria-label="Administration"><span className="nav-item active"><UserRound size={18} />Manage accounts</span><Link className="nav-item" to="/admin/issues"><Activity size={18} />Library health</Link></nav>
    </aside>
    <div className="main-shell">
      <header className="app-header">
        <Link className="mobile-brand" to="/library"><FolderOpen size={23} /><span>Media Explorer</span></Link>
        <div className="header-actions"><span className="header-account">{session?.name}</span><button className="button subtle" disabled={busy} onClick={logout}>Sign out</button></div>
      </header>
      <main className="library-main">
        <div className="section-heading accounts-heading">
          <div><h1>Accounts</h1><p className="section-note">Everyone with access to this media library.</p></div>
          <button className="button primary" onClick={() => { setDialogError(''); setDraft({ mode: 'create' }) }}><Plus size={17} />Create account</button>
        </div>
        {notice && <p className="toast-inline" role="status">{notice}</p>}
        {error && <div className="error-state" role="alert"><h2>Couldn’t load accounts</h2><p>{error}</p><button className="button primary" onClick={() => { setAccounts(null); void load() }}>Try again</button></div>}
        {!accounts && !error && <div className="loading-state" role="status"><span className="spinner" />Loading accounts…</div>}
        {accounts && !list.length && <div className="empty-state"><UserRound size={39} /><h3>No accounts yet</h3><p>Create the first account to share this library.</p></div>}
        {!!list.length && <section className="accounts-section" aria-label="Accounts">
          <div className="account-list-head"><span>ACCOUNT</span><span>ROLE</span><span>ACCESS</span><span className="sr-only">Actions</span></div>
          {list.map((account) => <article className="account-row" key={account.id}>
            <div className="account-identity">
              <span className="avatar"><UserRound size={18} /></span>
              <div><strong>{account.name}</strong><span className="account-username">{account.username}</span></div>
            </div>
            <span className={`role-badge ${account.role}`}><ShieldCheck size={13} />{account.role === 'admin' ? 'Admin' : 'Member'}</span>
            <span className={`state-badge ${account.active ? 'active' : 'disabled'}`}>{account.active ? 'Active' : 'Disabled'}</span>
            <div className="account-actions">
              <button className="button subtle compact" disabled={busy} onClick={() => { setDialogError(''); setDraft({ mode: 'password', account }) }}><KeyRound size={15} />Reset password</button>
              {account.active
                ? <button className="button subtle compact" disabled={busy} onClick={() => setConfirm({ account, action: 'disable' })}><UserX size={15} />Disable</button>
                : <button className="button subtle compact" disabled={busy} onClick={() => setConfirm({ account, action: 'enable' })}><ShieldCheck size={15} />Enable</button>}
              <button className="button subtle compact danger" disabled={busy} onClick={() => setConfirm({ account, action: 'delete' })}><Trash2 size={15} />Delete</button>
            </div>
            {account.role === 'admin' && account.active && activeAdmins === 1 && <p className="account-note">The last active administrator cannot be disabled or deleted.</p>}
          </article>)}
        </section>}
      </main>
    </div>
    {draft && <Dialog title={draft.mode === 'create' ? 'Create account' : 'Reset password'} onClose={() => setDraft(null)}>
      <form className="account-form" onSubmit={submitDraft}>
        {draft.mode === 'create' && <>
          <label className="field-label" htmlFor="new-username">Username</label>
          <div className="dialog-input"><input id="new-username" name="username" required autoCapitalize="none" spellCheck={false} placeholder="e.g. aisyah" /></div>
          <label className="field-label" htmlFor="new-name">Display name</label>
          <div className="dialog-input"><input id="new-name" name="name" required placeholder="e.g. Aisyah" /></div>
          <label className="field-label" htmlFor="new-role">Role</label>
          <div className="dialog-input"><select id="new-role" name="role" defaultValue="user"><option value="user">Member</option><option value="admin">Administrator</option></select></div>
        </>}
        <label className="field-label" htmlFor="new-password">Password</label>
        <div className="dialog-input"><input id="new-password" name="password" type="password" required minLength={12} autoComplete="new-password" placeholder="At least 12 characters" /></div>
        {dialogError && <p className="form-error" role="alert">{dialogError}</p>}
        <button className="button primary" type="submit" disabled={busy}>{busy ? <><span className="spinner" />Saving…</> : draft.mode === 'create' ? 'Create account' : 'Save password'}</button>
      </form>
    </Dialog>}
    {confirm && <Dialog title={confirm.action === 'delete' ? 'Delete account' : confirm.action === 'disable' ? 'Disable account' : 'Enable account'} onClose={() => setConfirm(null)}>
      <p className="muted">{confirm.action === 'delete'
        ? `${confirm.account.name} will lose access permanently. Their saved playback progress is removed too. Your media files stay untouched.`
        : confirm.action === 'disable'
          ? `${confirm.account.name} will be signed out immediately and cannot sign in until you enable the account again.`
          : `${confirm.account.name} will be able to sign in again.`}</p>
      <button className="button primary" disabled={busy} onClick={() => void apply(confirm.action, confirm.account)}>{confirm.action === 'delete' ? 'Delete account' : confirm.action === 'disable' ? 'Disable account' : 'Enable account'}</button>
      <button className="button subtle" disabled={busy} onClick={() => setConfirm(null)}>Cancel</button>
    </Dialog>}
  </div>
}
