import { lockEpoch, suspendPage } from '../appLock'

export type Media = {
  id: string
  folderId: string
  name: string
  kind: 'video' | 'image'
  image: string
  src: string
  resolution: string
  size: string
  format: string
  duration: number
  progress: number
  sprite?: Sprite
  playback?: Playback
}
export type Playback = {
  status: 'direct' | 'processing' | 'ready' | 'failed' | 'unsupported'
  src?: string
  reason?: string
}
/** Geometry of the server-rendered preview sheet: a cols x rows grid of frames
 *  sampled every `interval` seconds. `count` is how many tiles hold a real frame. */
export type Sprite = { url: string; cols: number; rows: number; count: number; interval: number }
export type Folder = { id: string; name: string; count: number; parentId: string }
export type Library = {
  folders: Folder[]
  items: Media[]
  continuing: Media[]
  folder: Folder | null
  navigation?: Folder[]
  breadcrumbs?: Folder[]
}
export type Session = { id: string; username: string; name: string; role: 'admin' | 'user'; lockEnabled?: boolean; hasPasskey?: boolean; passkeyAvailable?: boolean } | null
export type Account = { id: string; username: string; name: string; role: 'admin' | 'user'; active: boolean; createdAt: number }

export type IssueStage = 'probe' | 'sprite' | 'conversion' | 'queue' | 'skipped'
export type IssueItem = {
  id: string
  name: string
  path: string
  kind: 'video' | 'image'
  format: string
  size: number
  modifiedAt: number
  width: number
  height: number
  duration: number
  stage: IssueStage
  attempt: number
  maxAttempts: number | null
  nextRetryAt: number | null
  reason: string
  strategy?: string
  status?: string
  updatedAt: number | null
  skippedBy?: string
}
export type IssueCounts = { probe: number; sprite: number; conversions: number; queue: number; skipped: number; processing: number }
export type IssuePage = { tab: 'failed' | 'queue' | 'skipped'; counts: IssueCounts; total: number; items: IssueItem[]; limit: number; offset: number }

export const mocksEnabled = import.meta.env.VITE_ENABLE_MOCKS === 'true'

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  // Serialize cookie-changing responses across tabs so a late renewal cannot replace a fresh unlock.
  if (navigator.locks && /^\/(login|logout|lock\/(renew|lock|password|disable|register\/finish|enable\/finish|unlock\/finish))$/.test(path)) {
    const epoch = lockEpoch()
    return navigator.locks.request('mediaex:session-cookie', { signal: init?.signal ?? undefined }, () => {
      if (path === '/lock/renew' && (epoch !== lockEpoch() || document.visibilityState !== 'visible')) throw new DOMException('Renewal cancelled', 'AbortError')
      return send<T>(path, init)
    })
  }
  return send<T>(path, init)
}

async function send<T>(path: string, init?: RequestInit): Promise<T> {
  const epoch = lockEpoch()
  const headers = new Headers(init?.headers)
  headers.set('Content-Type', 'application/json')
  const response = await fetch(`/api${path}`, { ...init, headers })
  if (!response.ok) {
    const body = await response.json().catch(() => null)
    if (response.status === 423 && epoch === lockEpoch()) suspendPage(true)
    if (response.status === 401 && path !== '/login' && path !== '/session' && epoch === lockEpoch()) {
      window.dispatchEvent(new Event('mediaex:signed-out'))
    }
    throw new ApiError(response.status, body?.message || 'Something went wrong. Please try again.')
  }
  return response.json() as Promise<T>
}

export function time(seconds: number) {
  const value = Math.floor(Number.isFinite(seconds) ? Math.max(0, seconds) : 0)
  return `${Math.floor(value / 60).toString().padStart(2, '0')}:${(value % 60).toString().padStart(2, '0')}`
}
