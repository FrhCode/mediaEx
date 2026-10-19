import { useState } from 'react'
import type { FormEvent } from 'react'
import { ArrowRight, FolderOpen, LockKeyhole, UserRound } from 'lucide-react'
import { useLocation, useNavigate } from 'react-router-dom'
import { request } from '../api/client'
import type { Session } from '../api/client'
import Dialog from '../components/Dialog'

export default function LoginPage({ onLogin }: { onLogin: (session: Session) => void }) {
  const [show, setShow] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [help, setHelp] = useState(false)
  const navigate = useNavigate()
  const location = useLocation()
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const fields = new FormData(event.currentTarget)
    setBusy(true); setError('')
    try {
      const session = await request<Session>('/login', { method: 'POST', body: JSON.stringify({ username: fields.get('username'), password: fields.get('password') }) })
      onLogin(session)
      const target = location.state?.from
      navigate(typeof target === 'string' && (target.startsWith('/library') || target.startsWith('/media/')) ? target : '/library', { replace: true })
    } catch (error) { setError((error as Error).message) } finally { setBusy(false) }
  }
  return <main className="login-page">
    <div className="login-corner"><FolderOpen size={22} /><span>Media Explorer</span></div>
    <div className="login-card">
      <div className="login-mark"><FolderOpen size={48} strokeWidth={1.7} /></div>
      <div className="login-heading"><span className="eyebrow">YOUR COLLECTION, IN ONE PLACE</span><h1>Media Explorer</h1><p>Sign in to access your media library</p></div>
      <form onSubmit={submit} className="login-form">
        <label className="field-label" htmlFor="username">Username</label>
        <div className="login-input"><UserRound size={21} /><input id="username" name="username" placeholder="Username" required autoComplete="username" autoCapitalize="none" spellCheck={false} /></div>
        <label className="field-label" htmlFor="password">Password</label>
        <div className="login-input"><LockKeyhole size={21} /><input id="password" name="password" type={show ? 'text' : 'password'} placeholder="Password" required autoComplete="current-password" /><button type="button" onClick={() => setShow(!show)} aria-label={show ? 'Hide password' : 'Show password'}>{show ? 'Hide' : 'Show'}</button></div>
        {error && <p className="form-error" role="alert">{error}</p>}
        <button className="button primary signin" disabled={busy} type="submit">{busy ? <><span className="spinner" />Signing in…</> : <>Sign in<ArrowRight size={19} /></>}</button>
      </form>
      <button className="text-button help-link" onClick={() => setHelp(true)}>Need help signing in?</button>
    </div>
    <p className="login-footer">Your media. Your space.</p>
    {help && <Dialog title="Sign-in help" onClose={() => setHelp(false)}><p className="muted">Check your username and password, then try again. Passwords are case-sensitive.</p><button className="button primary" onClick={() => setHelp(false)}>Got it</button></Dialog>}
  </main>
}
