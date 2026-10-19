import { useState } from 'react'
import type { FormEvent } from 'react'
import { Fingerprint, LockKeyhole } from 'lucide-react'
import '../lock.css'

export default function LockScreen({ name, busy, error, passkeyAvailable, onPasskey, onPassword, onLogout }: { name: string; busy: boolean; error: string; passkeyAvailable: boolean; onPasskey: () => void; onPassword: (password: string) => void; onLogout: () => void }) {
  const [show, setShow] = useState(false)
  const [password, setPassword] = useState('')
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!password) return
    onPassword(password)
    setPassword('')
  }
  return <main className="login-page lock-page" aria-busy={busy}>
    <div className="login-corner"><LockKeyhole size={22} /><span>Media Explorer</span></div>
    <div className="login-card">
      <div className="login-mark"><LockKeyhole size={48} strokeWidth={1.7} /></div>
      <div className="login-heading">
        <span className="eyebrow">SESSION LOCKED</span>
        <h1>This session is locked</h1>
        <p>Signed in as {name}. Verify it is you to unlock.</p>
      </div>
      {error && <p className="form-error lock-error" role="alert">{error}</p>}
      {passkeyAvailable && <>
        <button className="button primary signin lock-passkey" type="button" disabled={busy} onClick={onPasskey}>{busy ? <><span className="spinner" />Verifying…</> : <><Fingerprint size={19} />Unlock with passkey</>}</button>
        <p className="lock-hint">Your device chooses how to verify — a passkey, or its own screen lock.</p>
        <p className="lock-divider"><span>or use your account password</span></p>
      </>}
      <form className={`login-form${passkeyAvailable ? ' lock-form-secondary' : ''}`} onSubmit={submit}>
        <label className="field-label" htmlFor="lock-password">Account password</label>
        <div className="login-input"><LockKeyhole size={21} /><input id="lock-password" name="password" type={show ? 'text' : 'password'} value={password} onChange={(event) => setPassword(event.target.value)} placeholder="Password" autoComplete="current-password" autoCapitalize="none" spellCheck={false} /><button type="button" onClick={() => setShow(!show)} aria-label={show ? 'Hide password' : 'Show password'}>{show ? 'Hide' : 'Show'}</button></div>
        <button className={`button ${passkeyAvailable ? 'subtle' : 'primary'} signin`} disabled={busy || !password} type="submit">{busy ? <><span className="spinner" />Unlocking…</> : 'Unlock'}</button>
      </form>
      <button className="text-button help-link" disabled={busy} onClick={onLogout}>Sign out</button>
    </div>
    <p className="login-footer">Your media. Your space.</p>
  </main>
}
