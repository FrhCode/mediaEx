export type Grant = { expiresAt: number }
export type PlaybackPosition = { itemId: string; seconds: number }
export type LockStatus = { lockEnabled: boolean; hasPasskey: boolean; passkeyAvailable: boolean; grantActive: boolean; expiresAt: number }

let enabled = false
let epoch = 0
let mode: 'open' | 'checking' | 'locked' = 'open'
let expiryTimer = 0
let capturePlayback: (() => PlaybackPosition | undefined) | undefined
let resume: PlaybackPosition | undefined
const listeners = new Set<() => void>()

export const subscribeLock = (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener) } }
export const lockSnapshot = () => `${mode}:${epoch}`
export const isLocked = () => mode !== 'open'
export const isChecking = () => mode === 'checking'
export const lockEpoch = () => epoch
export function playbackResume(itemId: string) {
  if (resume?.itemId !== itemId) return undefined
  const seconds = resume.seconds
  resume = undefined
  return seconds
}
export function registerPlayback(capture: () => PlaybackPosition | undefined) {
  capturePlayback = capture
  return () => { if (capturePlayback === capture) capturePlayback = undefined }
}
function publish() {
  document.documentElement.toggleAttribute('data-app-locked', isLocked())
  listeners.forEach(listener => listener())
}
function capture() {
  const captureCurrent = capturePlayback
  capturePlayback = undefined
  const position = captureCurrent?.()
  if (position) resume = position
  return position
}
function saveProgress(position?: PlaybackPosition) {
  if (!position) return
  void fetch(`/api/items/${encodeURIComponent(position.itemId)}/progress`, { method: 'PUT', keepalive: true, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ progress: Math.max(0, Math.round(position.seconds)) }) }).catch(() => undefined)
}
export function configureLock(policy: boolean, reset = false) {
  window.clearTimeout(expiryTimer)
  if (reset) resume = undefined
  epoch++
  enabled = policy
  mode = policy ? 'checking' : 'open'
  publish()
}
export function applyStatus(value: LockStatus, expectedEpoch: number) {
  if (epoch !== expectedEpoch || document.visibilityState !== 'visible') return false
  enabled = value.lockEnabled
  const active = value.grantActive && value.expiresAt * 1000 > Date.now()
  mode = !enabled || active ? 'open' : 'locked'
  window.clearTimeout(expiryTimer)
  if (enabled && active) expiryTimer = window.setTimeout(() => suspendPage(), Math.max(0, value.expiresAt * 1000 - Date.now()))
  publish()
  return true
}
export function renewGrant(value: Grant, expectedEpoch: number) {
  if (epoch !== expectedEpoch || isLocked() || document.visibilityState !== 'visible') return
  window.clearTimeout(expiryTimer)
  expiryTimer = window.setTimeout(() => suspendPage(), Math.max(0, value.expiresAt * 1000 - Date.now()))
}
export function lockPage() {
  if (!enabled) return
  window.clearTimeout(expiryTimer)
  epoch++
  mode = 'locked'
  document.documentElement.setAttribute('data-app-locked', '')
  const position = capture()
  publish()
  return position
}
export function suspendPage(force = false) {
  if (!enabled && !force) return
  if (force) enabled = true
  window.clearTimeout(expiryTimer)
  epoch++
  mode = 'checking'
  document.documentElement.setAttribute('data-app-locked', '')
  saveProgress(capture())
  publish()
}
export function startLockLifecycle() {
  document.addEventListener('visibilitychange', () => suspendPage())
  window.addEventListener('pagehide', () => suspendPage())
  window.addEventListener('pageshow', event => { if (event.persisted) suspendPage() })
}
