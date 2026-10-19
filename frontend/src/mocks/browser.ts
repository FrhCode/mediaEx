import { delay, http, HttpResponse } from 'msw'
import { setupWorker } from 'msw/browser'
import { folderNames, items } from './fixtures'
import type { Media } from '../api/client'

function read<T>(key: string, fallback: T): T {
  try { return JSON.parse(sessionStorage.getItem(key) || 'null') ?? fallback } catch { return fallback }
}
function save(key: string, value: unknown) {
  try { sessionStorage.setItem(key, JSON.stringify(value)) } catch { /* Storage is optional in the demo. */ }
}
let session = read<{ id: string; username: string; name: string; role: 'admin' | 'user' } | null>('media-demo-session', null)
const progress = read<Record<string, number>>('media-demo-progress', {})
let scenario = 'normal'
let preparedAt = 0
const folders = [
  ...folderNames.map((name) => ({ id: name.toLowerCase(), name, parentId: '' })),
  { id: 'trips', name: 'Trips', parentId: 'travel' },
]
function inFolder(itemFolder: string, parent: string): boolean {
  if (!parent) return true
  for (let current = itemFolder; current; current = folders.find((folder) => folder.id === current)?.parentId ?? '') {
    if (current === parent) return true
  }
  return false
}
const folderDTO = (folder: typeof folders[number]) => ({ ...folder, count: items.filter((item) => inFolder(item.folderId, folder.id)).length })
const withProgress = (): Media[] => items.map((item) => {
  const media: Media = { ...item, progress: progress[item.id] ?? item.progress }
  if (item.kind === 'video') media.playback = { status: 'direct' }
  if (item.id === 'drone' && scenario === 'converting') {
    media.playback = Date.now() < preparedAt ? { status: 'processing' } : { status: 'ready', src: '/media/demo.mp4?derived=drone' }
  }
  if (item.id === 'drone' && scenario === 'unplayable') {
    media.playback = { status: 'unsupported', reason: 'This file has no supported video stream.' }
  }
  return media
})
const unauthorized = () => HttpResponse.json({ message: 'Please sign in to continue.' }, { status: 401 })

type DemoIssue = {
  id: string; name: string; path: string; kind: 'video' | 'image'; format: string; size: number
  modifiedAt: number; width: number; height: number; duration: number
  stage: 'probe' | 'sprite' | 'conversion' | 'queue' | 'skipped'
  attempt: number; maxAttempts: number | null; nextRetryAt: number | null; reason: string
  strategy?: string; status?: string; updatedAt: number | null; skippedBy?: string
}
const now = () => Math.floor(Date.now() / 1000)
const demoIssues: DemoIssue[] = [
  { id: 'drone', name: 'Drone Footage.mp4', path: 'recordings/Drone Footage.mp4', kind: 'video', format: 'MP4', size: 7650000, modifiedAt: now() - 86400, width: 1280, height: 720, duration: 20.687, stage: 'conversion', attempt: 3, maxAttempts: 3, nextRetryAt: null, reason: 'convert: ffmpeg produced no output', strategy: 'transcode_full', status: 'failed', updatedAt: now() - 3600 },
  { id: 'holiday', name: 'My Holiday Recording 2026.mp4', path: 'recordings/My Holiday Recording 2026.mp4', kind: 'video', format: 'MP4', size: 7650000, modifiedAt: now() - 172800, width: 1280, height: 720, duration: 20.687, stage: 'sprite', attempt: 2, maxAttempts: 3, nextRetryAt: now() + 300, reason: 'sprite: context deadline exceeded', updatedAt: null },
  { id: 'forest', name: 'A walk in the forest.jpg', path: 'family/A walk in the forest.jpg', kind: 'image', format: 'JPG', size: 425000, modifiedAt: now() - 600, width: 1200, height: 800, duration: 0, stage: 'probe', attempt: 4, maxAttempts: null, nextRetryAt: now() + 21600, reason: 'probe failed: Invalid data found when processing input', updatedAt: null },
  { id: 'alpine', name: 'Alpine Views.mp4', path: 'Trips/Alpine Views.mp4', kind: 'video', format: 'MP4', size: 7650000, modifiedAt: now() - 7200, width: 1280, height: 720, duration: 20.687, stage: 'queue', attempt: 0, maxAttempts: 3, nextRetryAt: null, reason: 'The video codec (hevc) is not browser-compatible and is being converted.', strategy: 'transcode_full', status: 'processing', updatedAt: now() - 720 },
  { id: 'coast', name: 'A little escape.mp4', path: 'movies/A little escape.mp4', kind: 'video', format: 'MP4', size: 7650000, modifiedAt: now() - 90000, width: 1280, height: 720, duration: 20.687, stage: 'queue', attempt: 0, maxAttempts: 3, nextRetryAt: null, reason: 'The MKV container is not browser-friendly; the video is being repackaged as MP4.', strategy: 'remux', status: 'pending', updatedAt: now() - 120 },
  { id: 'island', name: 'Phi Phi Island.jpg', path: 'travel/Phi Phi Island.jpg', kind: 'image', format: 'JPG', size: 296000, modifiedAt: now() - 864000, width: 1200, height: 800, duration: 0, stage: 'skipped', attempt: 0, maxAttempts: null, nextRetryAt: null, reason: 'broken download, keep for now', updatedAt: now() - 86400, skippedBy: 'Explorer' },
]
function demoCounts() {
  const count = (stage: DemoIssue['stage']) => demoIssues.filter(item => item.stage === stage).length
  return {
    probe: count('probe'), sprite: count('sprite'), conversions: count('conversion'),
    queue: count('queue'), skipped: count('skipped'),
    processing: demoIssues.filter(item => item.stage === 'queue' && item.status === 'processing').length,
  }
}

export const worker = setupWorker(
  http.get('/api/session', async () => { await delay(150); return HttpResponse.json(session) }),
  http.post('/api/login', async ({ request }) => {
    await delay(500)
    const body = await request.json() as { username: string; password: string }
    if (body.username !== 'demo' || body.password !== 'demo123') return HttpResponse.json({ message: 'Invalid username or password.' }, { status: 401 })
    session = { id: 'demo', username: 'demo', name: 'Explorer', role: 'admin' }
    save('media-demo-session', session)
    return HttpResponse.json(session)
  }),
  http.post('/api/logout', () => { session = null; scenario = 'normal'; save('media-demo-session', null); return HttpResponse.json({ ok: true }) }),
  http.post('/api/scenario', async ({ request }) => {
    if (!session) return unauthorized()
    const body = await request.json() as { scenario: string }
    scenario = body.scenario
    preparedAt = Date.now() + 8000
    return HttpResponse.json({ scenario })
  }),
  http.get('/api/library', async ({ request }) => {
    if (!session) return unauthorized()
    await delay(scenario === 'slow' ? 2200 : 300)
    if (scenario === 'error') { scenario = 'normal'; return HttpResponse.json({ message: 'Unable to load your library. Your files are safe — try again.' }, { status: 503 }) }
    const url = new URL(request.url)
    const folderId = url.searchParams.get('folder') || ''
    const scope = url.searchParams.get('scope') || (folderId ? 'folder' : 'all')
    if (!['all', 'folder'].includes(scope) || (scope === 'all' && folderId)) return HttpResponse.json({ message: 'Invalid library scope.' }, { status: 400 })
    const query = (url.searchParams.get('q') || '').toLowerCase()
    const folder = folders.find((value) => value.id === folderId)
    if (folderId && !folder) return HttpResponse.json({ message: 'This folder could not be found.' }, { status: 404 })
    const breadcrumbs = []
    for (let current = folder; current; current = folders.find((value) => value.id === current?.parentId)) breadcrumbs.unshift(folderDTO(current))
    const all = withProgress()
    return HttpResponse.json({
      folders: query || scenario === 'empty' ? [] : folders.filter((value) => value.parentId === folderId).map(folderDTO),
      items: scenario === 'empty' ? [] : all.filter((item) => (query ? inFolder(item.folderId, folderId) : scope === 'all' || item.folderId === folderId) && item.name.toLowerCase().includes(query)),
      continuing: scenario === 'empty' ? [] : all.filter((item) => item.kind === 'video' && item.progress > 0 && item.progress < item.duration - 1),
      folder: folder ? folderDTO(folder) : null,
      navigation: scenario === 'empty' ? [] : folders.filter((value) => !value.parentId).map(folderDTO),
      breadcrumbs,
    })
  }),
  http.get('/api/items/:id', async ({ params }) => {
    if (!session) return unauthorized()
    await delay(200)
    const item = withProgress().find((value) => value.id === params.id)
    return item ? HttpResponse.json(item) : HttpResponse.json({ message: 'This file could not be found.' }, { status: 404 })
  }),
  http.put('/api/items/:id/progress', async ({ params, request }) => {
    if (!session) return unauthorized()
    const item = items.find((value) => value.id === params.id)
    const body = await request.json() as { progress: number }
    if (!item || item.kind !== 'video' || !Number.isFinite(body.progress)) return HttpResponse.json({ message: 'Invalid playback progress.' }, { status: 400 })
    progress[item.id] = Math.min(item.duration, Math.max(0, body.progress))
    save('media-demo-progress', progress)
    return HttpResponse.json({ ok: true })
  }),
  http.get('/api/admin/issues', async ({ request }) => {
    if (!session) return unauthorized()
    await delay(120)
    const url = new URL(request.url)
    const tab = url.searchParams.get('tab') || 'failed'
    const q = (url.searchParams.get('q') || '').toLowerCase()
    const limit = Number(url.searchParams.get('limit') || 50)
    const offset = Number(url.searchParams.get('offset') || 0)
    const inTab = (item: DemoIssue) => tab === 'failed' ? ['probe', 'sprite', 'conversion'].includes(item.stage) : tab === 'queue' ? item.stage === 'queue' : item.stage === 'skipped'
    const matched = demoIssues.filter(item => inTab(item) && (!q || item.name.toLowerCase().includes(q) || item.path.toLowerCase().includes(q)))
    return HttpResponse.json({ tab, counts: demoCounts(), total: matched.length, limit, offset, items: matched.slice(offset, offset + limit) })
  }),
  http.post('/api/admin/issues/:id/skip', async ({ params, request }) => {
    if (!session) return unauthorized()
    const body = await request.json().catch(() => ({})) as { reason?: string }
    const found = demoIssues.find(item => item.id === params.id)
    if (!found) return HttpResponse.json({ message: 'This media item could not be found.' }, { status: 404 })
    Object.assign(found, { stage: 'skipped', status: '', attempt: 0, nextRetryAt: null, reason: body.reason || '', skippedBy: session.name, updatedAt: now() })
    return HttpResponse.json({ ok: true })
  }),
  http.delete('/api/admin/issues/:id/skip', ({ params }) => {
    if (!session) return unauthorized()
    const index = demoIssues.findIndex(item => item.id === params.id && item.stage === 'skipped')
    if (index < 0) return HttpResponse.json({ message: 'This media item could not be found.' }, { status: 404 })
    demoIssues.splice(index, 1)
    return HttpResponse.json({ ok: true })
  }),
  http.post('/api/admin/issues/:id/retry', ({ params }) => {
    if (!session) return unauthorized()
    const found = demoIssues.find(item => item.id === params.id)
    if (!found) return HttpResponse.json({ message: 'This media item could not be found.' }, { status: 404 })
    if (found.stage === 'skipped') return HttpResponse.json({ message: 'This item is skipped. Restore it before trying again.' }, { status: 409 })
    if (found.stage === 'conversion') Object.assign(found, { stage: 'queue', status: 'pending', attempt: 0, reason: 'Waiting for conversion.', updatedAt: now() })
    else Object.assign(found, { attempt: 0, nextRetryAt: now() + 5 })
    return HttpResponse.json({ ok: true })
  }),
  http.post('/api/admin/issues/skip', async ({ request }) => {
    if (!session) return unauthorized()
    const body = await request.json().catch(() => ({})) as { ids?: string[]; reason?: string }
    let skipped = 0
    for (const id of body.ids || []) {
      const found = demoIssues.find(item => item.id === id)
      if (!found) continue
      Object.assign(found, { stage: 'skipped', status: '', attempt: 0, nextRetryAt: null, reason: body.reason || '', skippedBy: session.name, updatedAt: now() })
      skipped++
    }
    return HttpResponse.json({ ok: true, skipped })
  }),
  http.all('/api/*', () => HttpResponse.json({ message: 'This endpoint is not available in the local demo.' }, { status: 404 })),
)
