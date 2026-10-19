import { useId, useState } from 'react'
import type { FormEvent } from 'react'
import { Fingerprint, LockKeyhole } from 'lucide-react'
import type { Session } from '../api/client'
import '../lock.css'

export default function LockSettings({ session, busy, error, onEnable, onDisable, onLock }: { session: NonNullable<Session>; busy: boolean; error: string; onEnable: (password: string, useExisting: boolean) => void; onDisable: (password: string) => void; onLock: () => void }) {
  const titleId = useId()
  const enableId = useId()
  const disableId = useId()
  const [enablePassword, setEnablePassword] = useState('')
  const [disablePassword, setDisablePassword] = useState('')
  const lockEnabled = session.lockEnabled === true
  const hasPasskey = session.hasPasskey === true
  const passkeyAvailable = session.passkeyAvailable === true && !!window.PublicKeyCredential

  function enableWithPassword(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!enablePassword) return
    onEnable(enablePassword, false)
    setEnablePassword('')
  }
  function disableWithPassword(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!disablePassword) return
    onDisable(disablePassword)
    setDisablePassword('')
  }

  return <section className="lock-settings" aria-labelledby={titleId}>
    <div className="lock-settings-head">
      <h2 id={titleId}><LockKeyhole size={18} />Session lock</h2>
      <span className={`state-badge ${lockEnabled ? 'active' : 'disabled'}`}>{lockEnabled ? 'On for this session' : 'Off'}</span>
    </div>
    <p className="muted">Unlock stays active while this app is visible. Its 60-second permission is renewed every 15 seconds; leaving the app stops renewal and immediately hides your media. Refreshing or returning can continue without verification while permission is still valid. Tabs sharing this login share unlock permission, and Lock now locks them all. A new sign-in starts with the lock off; other devices are unaffected.</p>
    {error && <p className="form-error lock-error" role="alert">{error}</p>}
    <div className="lock-actions">
      {lockEnabled && <button className="button primary" disabled={busy} onClick={onLock}><LockKeyhole size={17} />Lock now</button>}
      {lockEnabled && <form className="lock-form" onSubmit={disableWithPassword}>
        <label className="field-label" htmlFor={disableId}>Turn off the session lock</label>
        <div className="dialog-input"><input id={disableId} type="password" value={disablePassword} onChange={(event) => setDisablePassword(event.target.value)} placeholder="Account password" autoComplete="current-password" /></div>
        <button className="button subtle" disabled={busy || !disablePassword} type="submit">Turn off</button>
      </form>}
      {!lockEnabled && !passkeyAvailable && <p className="lock-requirement">Passkeys need a trusted HTTPS origin. Set PUBLIC_ORIGIN to your HTTPS domain — localhost for development — before this lock can be turned on.</p>}
      {!lockEnabled && passkeyAvailable && hasPasskey && <button className="button primary" disabled={busy} onClick={() => onEnable('', true)}><Fingerprint size={17} />Use an existing passkey to turn on</button>}
      {!lockEnabled && passkeyAvailable && <form className="lock-form" onSubmit={enableWithPassword}>
        <label className="field-label" htmlFor={enableId}>{hasPasskey ? 'Add another passkey' : 'Create a passkey'}</label>
        <div className="dialog-input"><input id={enableId} type="password" value={enablePassword} onChange={(event) => setEnablePassword(event.target.value)} placeholder="Account password" autoComplete="current-password" /></div>
        {!hasPasskey && <p className="lock-note">A passkey is created on this device and your device confirms it. Your account password stays required to turn the lock off.</p>}
        <button className={`button ${hasPasskey ? 'subtle' : 'primary'}`} disabled={busy || !enablePassword} type="submit">{hasPasskey ? 'Add passkey' : 'Create passkey and turn on'}</button>
      </form>}
      {lockEnabled && <p className="lock-note">Turning the lock off ends it for this session only. Passkeys you have registered stay on your account for other sessions.</p>}
    </div>
  </section>
}
