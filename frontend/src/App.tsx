import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { ApiError, mocksEnabled, request } from './api/client'
import type { Session } from './api/client'
import LoginPage from './pages/LoginPage'
import LibraryPage from './pages/LibraryPage'
import MediaPage from './pages/MediaPage'
import AdminUsersPage from './pages/AdminUsersPage'
import AdminIssuesPage from './pages/AdminIssuesPage'
import TestingBubble from './components/TestingBubble'
import LockScreen from './components/LockScreen'
import { applyStatus, configureLock, isChecking, isLocked, lockEpoch, lockPage, lockSnapshot, renewGrant, subscribeLock, suspendPage } from './appLock'
import type { Grant, LockStatus } from './appLock'
import { verifyPasskey } from './passkey'
import './App.css'
import './lock.css'

function Application() {
  const [session, setSession] = useState<Session>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(false)
  const location = useLocation()
  const [scenario, setScenario] = useState('normal')
  const [libraryRevision, setLibraryRevision] = useState(0)
  const snapshot = useSyncExternalStore(subscribeLock, lockSnapshot)
  const locked = isLocked()
  const checking = isChecking()
  const [statusErrorEpoch, setStatusErrorEpoch] = useState<number | null>(null)
  const sessionID = session?.id
  const [lockPending, setLockPending] = useState(false)
  const [lockUnconfirmed, setLockUnconfirmed] = useState(false)
  const [lockBusy, setLockBusy] = useState(false)
  const [lockError, setLockError] = useState('')
  const verification = useRef<AbortController | null>(null)
  const identityEpoch = useRef(0)
  function acceptSession(value: Session) {
    identityEpoch.current++
    verification.current?.abort()
    configureLock(!!value?.lockEnabled, true)
    setSession(value)
    setLockUnconfirmed(false)
    setLockError('')
  }
  async function verify(action: 'unlock' | 'register' | 'enable' | 'password' | 'disable', password = '') {
    if (verification.current) return
    const controller = new AbortController()
    verification.current = controller
    const identity = identityEpoch.current
    setLockBusy(true)
    setLockError('')
    try {
      if (action === 'disable') {
        const result = await request<{ session: NonNullable<Session> }>('/lock/disable', { method: 'POST', body: JSON.stringify({ password }), signal: controller.signal })
        if (identity !== identityEpoch.current) return
        configureLock(false)
        setSession(result.session)
      } else {
        const result = action === 'password'
          ? await request<Grant>('/lock/password', { method: 'POST', body: JSON.stringify({ password }), signal: controller.signal })
          : await verifyPasskey(action, password, controller.signal)
        if (controller.signal.aborted || identity !== identityEpoch.current) return
        configureLock(true)
        setSession(value => value ? { ...value, ...('session' in result ? result.session as NonNullable<Session> : {}), lockEnabled: true } : value)
      }
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 409 && !controller.signal.aborted) {
        const state = await request<Session>('/session').catch(() => null)
        if (identity === identityEpoch.current && state && !state.lockEnabled) { configureLock(false); setSession(state) }
      }
      if (!controller.signal.aborted) setLockError(cause instanceof Error ? cause.message : 'Verification failed. Please try again.')
      // Activation might have committed before a connection was interrupted.
      if (action === 'register' || action === 'enable') {
        const state = await request<Session>('/session').catch(() => null)
        if (!controller.signal.aborted && identity === identityEpoch.current && state?.lockEnabled) { configureLock(true); setSession(state) }
      }
    } finally {
      if (verification.current === controller) { verification.current = null; setLockBusy(false) }
    }
  }
  async function manualLock() {
    identityEpoch.current++
    verification.current?.abort()
    setLockPending(true)
    setLockUnconfirmed(true)
    setLockError('')
    const progress = lockPage()
    try {
      try {
        await request('/lock/lock', { method: 'POST', body: JSON.stringify(progress ? { progress } : {}) })
      } catch (cause) {
        if (!(cause instanceof ApiError && cause.status === 423 && progress)) throw cause
        await request('/lock/lock', { method: 'POST', body: '{}' })
      }
      setLockUnconfirmed(false)
    } catch {
      setLockError('The page is concealed, but the server could not confirm locking other tabs. Retry Lock now.')
    } finally { setLockPending(false) }
  }
  async function changeScenario(value: string) {
    await request('/scenario', { method: 'POST', body: JSON.stringify({ scenario: value }) })
    setScenario(value)
    setLibraryRevision((revision) => revision + 1)
  }
  function logout() {
    setScenario('normal')
    acceptSession(null)
  }
  useEffect(() => {
    let active = true
    request<Session>('/session').then((value) => { if (active) { configureLock(!!value?.lockEnabled, true); setSession(value) } }).catch(() => { if (active) setError(true) }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [])
  useEffect(() => {
    const signedOut = () => { identityEpoch.current++; verification.current?.abort(); configureLock(false, true); setSession(null) }
    window.addEventListener('mediaex:signed-out', signedOut)
    return () => window.removeEventListener('mediaex:signed-out', signedOut)
  }, [])
  useEffect(() => {
    if (!sessionID || !checking || lockPending || lockUnconfirmed || document.visibilityState !== 'visible') return
    let active = true
    const epoch = lockEpoch()
    const identity = identityEpoch.current
    request<LockStatus>('/lock/status').then(async value => {
      if (!active || identity !== identityEpoch.current || epoch !== lockEpoch() || document.visibilityState !== 'visible') return
      if (value.lockEnabled && value.grantActive && value.expiresAt * 1000 > Date.now()) {
        try {
          const renewed = await request<Grant>('/lock/renew', { method: 'POST', body: '{}' })
          value = { ...value, expiresAt: renewed.expiresAt }
        } catch (cause) {
          if (!(cause instanceof ApiError && cause.status === 409)) throw cause
          value = await request<LockStatus>('/lock/status')
        }
      }
      if (!active || identity !== identityEpoch.current || !applyStatus(value, epoch)) return
      setSession(current => current ? { ...current, lockEnabled: value.lockEnabled, hasPasskey: value.hasPasskey, passkeyAvailable: value.passkeyAvailable } : current)
    }).catch(() => { if (active && epoch === lockEpoch()) setStatusErrorEpoch(epoch) })
    return () => { active = false }
  }, [sessionID, checking, snapshot, lockPending, lockUnconfirmed])
  useEffect(() => {
    if (!session?.lockEnabled || locked || document.visibilityState !== 'visible') return
    let active = true
    let pending = false
    const epoch = lockEpoch()
    const timer = window.setInterval(async () => {
      if (pending || document.visibilityState !== 'visible') return
      pending = true
      try {
        const value = await request<Grant>('/lock/renew', { method: 'POST', body: '{}' })
        if (active) renewGrant(value, epoch)
      } catch (cause) {
        if (active && epoch === lockEpoch() && cause instanceof ApiError && cause.status === 409) suspendPage()
      } finally { pending = false }
    }, 15000)
    return () => { active = false; window.clearInterval(timer) }
  }, [session?.lockEnabled, locked, snapshot])
  if (loading) return <div className="app-loading"><span className="spinner" />Opening your library…</div>
  if (error) return <div className="error-state"><h1>Unable to open your library</h1><button className="button primary" onClick={() => window.location.reload()}>Reload</button></div>
  if (!session && location.pathname !== '/login') return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />
  if (session && lockUnconfirmed) return <div className="app-loading">{lockPending ? <><span className="spinner" />Locking this session…</> : <><p role="alert">{lockError}</p><button className="button primary" onClick={() => void manualLock()}>Retry Lock now</button></>}</div>
  if (session && checking) return <div className="app-loading">{statusErrorEpoch === lockEpoch() ? <><p>Unable to verify this session. Your content stays hidden.</p><button className="button primary" onClick={() => suspendPage()}>Retry</button></> : <><span className="spinner" />Checking your session…</>}</div>
  if (session && locked) return <LockScreen name={session.name} busy={lockBusy} error={lockError} passkeyAvailable={!!session.passkeyAvailable && !!window.PublicKeyCredential} onPasskey={() => void verify('unlock')} onPassword={password => void verify('password', password)} onLogout={() => { void request('/logout', { method: 'POST' }).then(logout).catch(cause => setLockError(cause.message)) }} />
  const admin = session?.role === 'admin'
  return <>
    <div className="private-content"><Routes>
      <Route path="/login" element={session ? <Navigate to="/library" replace /> : <LoginPage onLogin={acceptSession} />} />
      <Route path="/library" element={<LibraryPage session={session!} lockBusy={lockBusy} lockError={lockError} onEnableLock={(password, existing) => void verify(existing ? 'enable' : 'register', password)} onDisableLock={password => void verify('disable', password)} onLock={() => void manualLock()} onLogout={logout} revision={libraryRevision} onRetry={() => { if (mocksEnabled) setScenario('normal') }} />} />
      <Route path="/media/:id" element={<MediaPage key={location.pathname} />} />
      <Route path="/admin/users" element={admin ? <AdminUsersPage session={session!} onLogout={logout} /> : <Navigate to="/library" replace />} />
      <Route path="/admin/issues" element={admin ? <AdminIssuesPage session={session!} onLogout={logout} /> : <Navigate to="/library" replace />} />
      <Route path="*" element={<Navigate to="/library" replace />} />
    </Routes></div>
    {mocksEnabled && <TestingBubble session={session} scenario={scenario} onScenario={changeScenario} />}
  </>
}
export default function App() { return <BrowserRouter><Application /></BrowserRouter> }
