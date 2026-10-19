import { useCallback, useEffect, useRef, useState } from 'react'
import type { ChangeEvent, KeyboardEvent, PointerEvent } from 'react'
import {
  ChevronLeft,
  LoaderCircle,
  Maximize,
  Minimize,
  Pause,
  Play,
  RotateCcw,
  RotateCw,
  Settings,
  Volume2,
  VolumeX,
} from 'lucide-react'
import { Link, useParams } from 'react-router-dom'
import { request, time } from '../api/client'
import type { Media } from '../api/client'

import PlayerSettings from '../components/PlayerSettings'
import { isLocked, playbackResume, registerPlayback } from '../appLock'
const SAVE_EVERY_MS = 3000
const SKIP_SECONDS = 10
const TAP_WINDOW_MS = 350
const CONTROLS_HIDE_MS = 3000
const HOLD_SPEED_MS = 500
const BUFFERING_DELAY_MS = 300
// Must match the .seek-preview-frame rule in App.css and spriteTileW/H on the server.
const PREVIEW_FRAME_W = 160
const PREVIEW_FRAME_H = 90
const PREVIEW_GAP = 10

async function putProgress(itemId: string, seconds: number) {
  try {
    await request(`/items/${encodeURIComponent(itemId)}/progress`, {
      method: 'PUT',
      body: JSON.stringify({ progress: Math.max(0, Math.round(seconds)) }),
    })
  } catch {
    /* Best effort: a rejected progress update must not interrupt playback. */
  }
}

// A video wants landscape, but only Android Chrome honours this and only while the
// element is fullscreen. Desktop and iOS reject it, which must not be reported as a
// fullscreen failure because entering fullscreen has already succeeded by then.
async function lockLandscape() {
  try {
    await screen.orientation?.lock?.('landscape')
  } catch {
    /* Unsupported here, so the phone keeps its own orientation. */
  }
}

export default function MediaPage() {
  const { id } = useParams()
  const [data, setData] = useState<{ key: string; media: Media | null; error: string } | null>(null)
  const [attempt, setAttempt] = useState(0)
  const [playing, setPlaying] = useState(false)
  const [current, setCurrent] = useState(0)
  const [duration, setDuration] = useState(0)
  const [volume, setVolume] = useState(1)
  const [muted, setMuted] = useState(false)
  const [rate, setRate] = useState(1)
  const [fullscreen, setFullscreen] = useState(false)
  const [settings, setSettings] = useState<HTMLButtonElement | null>(null)
  const [failed, setFailed] = useState(false)
  const [failure, setFailure] = useState<{ message: string; retryable: boolean } | null>(null)
  const [videoAttempt, setVideoAttempt] = useState(0)
  const retryPositionRef = useRef<number | undefined>(undefined)
  const [buffering, setBuffering] = useState(false)
  const [speedBoost, setSpeedBoost] = useState(false)
  const bufferingTimer = useRef(0)
  const holdTimer = useRef(0)
  const boostRef = useRef<{ video: HTMLVideoElement; rate: number } | null>(null)
  const [notice, setNotice] = useState('')
  const [controlsVisible, setControlsVisible] = useState(true)
  const [interaction, setInteraction] = useState(0)
  const [aspectRatio, setAspectRatio] = useState('16 / 9')
  const [skipFeedback, setSkipFeedback] = useState<{ direction: 'back' | 'forward'; seconds: number } | null>(null)
  const tapTimer = useRef(0)
  const [tapPending, setTapPending] = useState(false)
  // The frame the pointer is hovering, not a seek: this must never touch
  // video.currentTime or hovering the bar would stutter playback.
  const [preview, setPreview] = useState<{ time: number; left: number; top: number } | null>(null)
  const [spriteOk, setSpriteOk] = useState(false)
  // True from the moment the scrubber is grabbed until the pointer is released, which
  // is not the same as "an event just fired": a slow drag, or a finger held still
  // mid-drag, produces no events at all and the bar used to fade out underneath it.
  const [seeking, setSeeking] = useState(false)
  const tapStart = useRef<{ id: number; x: number; y: number; time: number } | null>(null)
  const lastTap = useRef<{ x: number; y: number; time: number; direction: 'back' | 'forward'; seconds: number } | null>(null)
  const controlsRef = useRef<HTMLDivElement>(null)
  // Touch input on Android can leave :focus-visible on the button that was tapped, so
  // focus alone cannot tell a keyboard user apart from a thumb.
  const keyboardNavRef = useRef(false)
  const hideTimer = useRef(0)
  const cursorPosition = useRef<{ x: number; y: number } | null>(null)

  const stageRef = useRef<HTMLDivElement>(null)
  const videoRef = useRef<HTMLVideoElement>(null)
  const secondsRef = useRef(0)
  const sentRef = useRef(-1)
  const loadedRef = useRef(false)
  const resumedRef = useRef(false)

  // The key travels with the response so a stale attempt can never overwrite a newer one.
  const pending = `${id ?? ''}:${attempt}`

  useEffect(() => {
    if (!id) return
    const controller = new AbortController()
    request<Media>(`/items/${encodeURIComponent(id)}`, { signal: controller.signal })
      .then((value) => {
        if (controller.signal.aborted) return
        setCurrent(0)
        setDuration(0)
        setPlaying(false)
        setFailed(false)
        setFailure(null)
        retryPositionRef.current = undefined
        setData({ key: pending, media: value, error: '' })
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return
        setData({ key: pending, media: null, error: cause instanceof Error ? cause.message : 'Something went wrong. Please try again.' })
      })
    return () => controller.abort()
  }, [id, pending])

  const settled = data && data.key === pending ? data : null
  const media = !id ? null : settled?.media ?? null
  const error = !id ? 'This item could not be found.' : settled?.error ?? ''
  const processing = media?.playback?.status === 'processing'
  const backendFailure = media?.playback?.status === 'failed' || media?.playback?.status === 'unsupported'
  const blocked = processing || backendFailure || !!failure
  const source = processing || backendFailure ? undefined : media?.playback?.status === 'ready' ? media.playback.src : media?.src

  useEffect(() => {
    if (!id || !processing) return
    let timer = 0
    let controller: AbortController | undefined
    const schedule = () => { timer = window.setTimeout(poll, 5000) }
    const poll = async () => {
      if (document.hidden) return
      const active = new AbortController()
      controller = active
      try {
        const value = await request<Media>(`/items/${encodeURIComponent(id)}`, { signal: active.signal })
        if (active.signal.aborted) return
        if (value.playback?.status !== 'processing') {
          setData((previous) => previous?.key === pending ? { ...previous, media: value } : previous)
          return
        }
      } catch {
        if (active.signal.aborted) return
      }
      schedule()
    }
    const visibility = () => {
      window.clearTimeout(timer)
      controller?.abort()
      if (!document.hidden) void poll()
    }
    if (!document.hidden) schedule()
    document.addEventListener('visibilitychange', visibility)
    return () => {
      window.clearTimeout(timer)
      controller?.abort()
      document.removeEventListener('visibilitychange', visibility)
    }
  }, [id, pending, processing])

  useEffect(() => {
    if (!notice) return
    const timer = window.setTimeout(() => setNotice(''), 3200)
    return () => window.clearTimeout(timer)
  }, [notice])

  useEffect(() => {
    if (!skipFeedback) return
    const timer = window.setTimeout(() => setSkipFeedback(null), 1500)
    return () => window.clearTimeout(timer)
  }, [skipFeedback])

  useEffect(() => {
    const sync = () => {
      const active = document.fullscreenElement === stageRef.current
      setFullscreen(active)
      // Releasing the lock on any exit — including the system back gesture — keeps the
      // rest of the app upright.
      if (!active) screen.orientation?.unlock?.()
    }
    document.addEventListener('fullscreenchange', sync)
    return () => document.removeEventListener('fullscreenchange', sync)
  }, [])

  const sendProgress = useCallback(() => {
    if (isLocked() || !media || media.kind !== 'video' || !loadedRef.current) return
    const value = Math.round(secondsRef.current)
    if (value === sentRef.current) return
    sentRef.current = value
    void putProgress(media.id, value)
  }, [media])

  useEffect(() => {
    if (!playing) return
    const timer = window.setInterval(sendProgress, SAVE_EVERY_MS)
    return () => window.clearInterval(timer)
  }, [playing, sendProgress])

  useEffect(() => {
    const itemId = media && media.kind === 'video' ? media.id : ''
    return () => {
      if (isLocked() || !itemId || !loadedRef.current) return
      const value = Math.round(secondsRef.current)
      if (value === sentRef.current) return
      sentRef.current = value
      void putProgress(itemId, value)
    }
  }, [media])

  // Runs after the cleanup above, so the outgoing item keeps its own second count.
  useEffect(() => {
    secondsRef.current = 0
    sentRef.current = -1
    loadedRef.current = false
    resumedRef.current = false
  }, [media])

  const stopBuffering = useCallback(() => {
    window.clearTimeout(bufferingTimer.current)
    bufferingTimer.current = 0
    setBuffering(false)
  }, [])

  const releaseVideoPointer = useCallback(() => {
    window.clearTimeout(holdTimer.current)
    holdTimer.current = 0
    tapStart.current = null
    const boost = boostRef.current
    boostRef.current = null
    if (boost) boost.video.playbackRate = boost.rate
    setSpeedBoost(false)
  }, [])

  const cancelVideoGesture = useCallback(() => {
    releaseVideoPointer()
    window.clearTimeout(tapTimer.current)
    tapTimer.current = 0
    lastTap.current = null
    setTapPending(false)
  }, [releaseVideoPointer])

  useEffect(() => {
    const cancel = () => {
      if (tapStart.current || boostRef.current) cancelVideoGesture()
    }
    const onVisibility = () => {
      if (document.hidden) cancelVideoGesture()
    }
    window.addEventListener('pointerup', cancel)
    window.addEventListener('pointercancel', cancelVideoGesture)
    window.addEventListener('blur', cancelVideoGesture)
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      cancelVideoGesture()
      stopBuffering()
      window.removeEventListener('pointerup', cancel)
      window.removeEventListener('pointercancel', cancelVideoGesture)
      window.removeEventListener('blur', cancelVideoGesture)
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [media, cancelVideoGesture, stopBuffering])

  useEffect(() => registerPlayback(() => {
    cancelVideoGesture()
    stopBuffering()
    const video = videoRef.current
    if (!media || media.kind !== 'video' || !video) return
    const position = loadedRef.current ? video.currentTime : media.progress
    video.pause()
    video.removeAttribute('src')
    video.removeAttribute('poster')
    video.load()
    if (document.fullscreenElement) void document.exitFullscreen().catch(() => undefined)
    return { itemId: media.id, seconds: position }
  }), [media, cancelVideoGesture, stopBuffering])

  function startBuffering() {
    const video = videoRef.current
    if (!video || video.paused || video.ended || video.error || bufferingTimer.current) return
    bufferingTimer.current = window.setTimeout(() => {
      if (!video.paused && !video.ended && !video.error) setBuffering(true)
    }, BUFFERING_DELAY_MS)
  }

  const showControls = controlsVisible || !playing || !!settings || blocked

  const closeSettings = useCallback(() => {
    setSettings(null)
    setControlsVisible(true)
    setInteraction((value) => value + 1)
  }, [])

  function revealControls() {
    setControlsVisible(true)
    setInteraction((value) => value + 1)
  }

  function toggleControls() {
    if (!playing || settings || blocked) {
      revealControls()
      return
    }
    setControlsVisible((visible) => !visible)
    setPreview(null)
    setInteraction((value) => value + 1)
  }

  function revealForCursor(event: PointerEvent<HTMLDivElement>) {
    if (event.pointerType !== 'mouse' && event.pointerType !== 'pen') return
    const previous = cursorPosition.current
    cursorPosition.current = { x: event.clientX, y: event.clientY }
    if (!previous || previous.x !== event.clientX || previous.y !== event.clientY) revealControls()
  }

  // The preview lives outside the control bar, so letting the bar fade while one is
  // open leaves a frame floating over the video with no controls under it.
  const previewOpen = preview !== null

  useEffect(() => {
    if (!seeking) return
    const stop = () => { setSeeking(false); setPreview(null) }
    window.addEventListener('pointerup', stop)
    window.addEventListener('pointercancel', stop)
    return () => {
      window.removeEventListener('pointerup', stop)
      window.removeEventListener('pointercancel', stop)
    }
  }, [seeking])

  useEffect(() => {
    if (!playing || settings || blocked || seeking || previewOpen || tapPending) return
    // Hiding the bar applies `inert`, which blurs whatever held focus inside it, so a
    // keyboard user who is still tabbing through the controls keeps it up. Re-arm on
    // every check: returning without rescheduling stranded the bar on screen for the
    // rest of the session the first time the guard tripped.
    const schedule = () => {
      hideTimer.current = window.setTimeout(() => {
        const tabbing = keyboardNavRef.current && controlsRef.current?.querySelector(':focus-visible')
        if (tabbing) schedule()
        else setControlsVisible(false)
      }, CONTROLS_HIDE_MS)
    }
    schedule()
    return () => window.clearTimeout(hideTimer.current)
  }, [playing, settings, blocked, interaction, seeking, previewOpen, tapPending])

  const totalDuration = Number.isFinite(duration) && duration > 0 ? duration : media?.duration ?? 0

  function togglePlay() {
    const video = videoRef.current
    if (!video || blocked) return
    if (video.paused) void video.play().catch(() => setNotice('Playback could not start. Try reloading the video.'))
    else video.pause()
  }

  function seekTo(seconds: number) {
    const video = videoRef.current
    if (!video || blocked || !Number.isFinite(seconds)) return
    const limit = Number.isFinite(video.duration) ? video.duration : seconds
    const next = Math.min(Math.max(0, seconds), limit)
    video.currentTime = next
    secondsRef.current = next
    setCurrent(next)
  }

  function startVideoTap(event: PointerEvent<HTMLVideoElement>) {
    if (!event.isPrimary || event.button !== 0 || settings || blocked) {
      cancelVideoGesture()
      return
    }
    releaseVideoPointer()
    window.clearTimeout(tapTimer.current)
    tapTimer.current = 0
    setTapPending(true)
    setInteraction((value) => value + 1)
    tapStart.current = { id: event.pointerId, x: event.clientX, y: event.clientY, time: event.timeStamp }
    const video = event.currentTarget
    if (video.paused || video.ended) return
    holdTimer.current = window.setTimeout(() => {
      if (!tapStart.current || video.paused || video.ended || video.error) return
      lastTap.current = null
      boostRef.current = { video, rate: video.playbackRate }
      video.playbackRate = 2
      setSpeedBoost(true)
    }, HOLD_SPEED_MS)
  }

  function moveVideoTap(event: PointerEvent<HTMLVideoElement>) {
    const start = tapStart.current
    if (start?.id === event.pointerId && Math.hypot(event.clientX - start.x, event.clientY - start.y) > 20) cancelVideoGesture()
  }

  function finishVideoTap(event: PointerEvent<HTMLVideoElement>) {
    const start = tapStart.current
    const boosted = boostRef.current !== null
    const previous = lastTap.current
    releaseVideoPointer()
    if (boosted || !start || start.id !== event.pointerId || settings || blocked) {
      cancelVideoGesture()
      return
    }
    if (event.timeStamp - start.time > TAP_WINDOW_MS || Math.hypot(event.clientX - start.x, event.clientY - start.y) > 20) {
      cancelVideoGesture()
      return
    }
    const bounds = event.currentTarget.getBoundingClientRect()
    const direction = event.clientX < bounds.left + bounds.width / 2 ? 'back' : 'forward'
    const continuing = previous && previous.direction === direction && event.timeStamp - previous.time <= TAP_WINDOW_MS && Math.hypot(event.clientX - previous.x, event.clientY - previous.y) <= 48
    const seconds = continuing ? previous.seconds + SKIP_SECONDS : 0
    lastTap.current = { x: event.clientX, y: event.clientY, time: event.timeStamp, direction, seconds }
    if (seconds > 0) {
      seekTo(event.currentTarget.currentTime + (direction === 'forward' ? SKIP_SECONDS : -SKIP_SECONDS))
      setSkipFeedback({ direction, seconds })
    }
    setTapPending(true)
    setInteraction((value) => value + 1)
    tapTimer.current = window.setTimeout(() => {
      tapTimer.current = 0
      lastTap.current = null
      setTapPending(false)
      if (seconds === 0) toggleControls()
    }, TAP_WINDOW_MS)
  }

  function adjustVolume(delta: number) {
    const video = videoRef.current
    if (!video) return
    const next = Math.min(1, Math.max(0, Math.round((video.volume + delta) * 100) / 100))
    video.volume = next
    if (next > 0) video.muted = false
    setVolume(next)
    setMuted(video.muted)
  }

  function toggleMute() {
    const video = videoRef.current
    if (!video) return
    video.muted = !video.muted
    if (!video.muted && video.volume === 0) video.volume = 0.6
    setMuted(video.muted)
    setVolume(video.volume)
  }

  function onLoadedMetadata() {
    const video = videoRef.current
    if (!video || !media) return
    setDuration(Number.isFinite(video.duration) ? video.duration : 0)
    if (video.videoWidth && video.videoHeight) setAspectRatio(`${video.videoWidth} / ${video.videoHeight}`)
    if (resumedRef.current) return
    resumedRef.current = true
    loadedRef.current = true
    const limit = Number.isFinite(video.duration) ? video.duration : media.duration
    const retryPosition = retryPositionRef.current
    retryPositionRef.current = undefined
    const preserved = playbackResume(media.id)
    const start = Math.min(Math.max(0, retryPosition ?? preserved ?? media.progress), Math.max(0, limit - 1))
    sentRef.current = Math.round(start)
    if (start > 0) {
      video.currentTime = start
      secondsRef.current = start
      setCurrent(start)
    }
    video.volume = volume
    video.muted = muted
    video.playbackRate = rate
    if (retryPosition !== undefined) {
      void video.play().catch(() => setNotice('Press Play to continue from your last position.'))
    } else if (preserved === undefined) startPlayback(video)
  }

  // Opening a video should start it. Browsers may still refuse autoplay, in which
  // case the player stays paused on the resume point and the play button works.
  function startPlayback(video: HTMLVideoElement) {
    void video.play().catch(() => undefined)
  }

  function onTimeUpdate() {
    const video = videoRef.current
    if (!video) return
    secondsRef.current = video.currentTime
    setCurrent(video.currentTime)
  }

  function onSeek(event: ChangeEvent<HTMLInputElement>) {
    seekTo(Number(event.target.value))
  }

  // Places the hover preview for a pointer at clientX over the scrubber. Purely
  // observational — it reads geometry and sets state, never the video element.
  function updatePreview(clientX: number, input: HTMLInputElement) {
    const plan = media?.sprite
    const stage = stageRef.current
    if (!plan || !spriteOk || !stage) return
    const rect = input.getBoundingClientRect()
    const stageRect = stage.getBoundingClientRect()
    const ratio = Math.min(1, Math.max(0, (clientX - rect.left) / rect.width))
    const span = totalDuration || plan.count * plan.interval
    const height = PREVIEW_FRAME_H + 20
    // Above the bar by default; on a short phone stage there is no room there, and
    // flipping below keeps it clear of the thumb rather than under it.
    let top = rect.top - stageRect.top - height - PREVIEW_GAP
    if (top < 8) top = rect.bottom - stageRect.top + PREVIEW_GAP
    const half = PREVIEW_FRAME_W / 2
    const left = Math.min(Math.max(rect.left - stageRect.left + ratio * rect.width, half), stageRect.width - half)
    setPreview({ time: ratio * span, left, top })
  }

  // Touch drags do not reliably report clientX, and the input's own value is the
  // position that will actually be seeked, so derive the pointer from it.
  function updatePreviewFromValue(input: HTMLInputElement) {
    const span = totalDuration
    if (!span) return
    const ratio = Math.min(1, Math.max(0, Number(input.value) / span))
    const rect = input.getBoundingClientRect()
    updatePreview(rect.left + ratio * rect.width, input)
  }

  function onVolume(event: ChangeEvent<HTMLInputElement>) {
    const video = videoRef.current
    const value = Number(event.target.value)
    if (!video || !Number.isFinite(value)) return
    video.volume = value
    video.muted = value === 0
    setVolume(value)
    setMuted(video.muted)
  }

  function syncVolume() {
    const video = videoRef.current
    if (!video) return
    setVolume(video.volume)
    setMuted(video.muted)
  }

  function onRate(value: number) {
    const video = videoRef.current
    if (!video) return
    video.playbackRate = value
    setRate(value)
  }

  function onVideoError() {
    if (isLocked()) return
    const video = videoRef.current
    if (!video || video.error?.code === 1) return
    cancelVideoGesture()
    stopBuffering()
    video.pause()
    setPlaying(false)
    setSettings(null)
    setPreview(null)
    setSeeking(false)
    retryPositionRef.current = loadedRef.current ? video.currentTime : retryPositionRef.current ?? media?.progress ?? 0
    secondsRef.current = retryPositionRef.current
    sendProgress()
    const code = video.error?.code
    const message = code === 2 ? 'The connection was interrupted. Try loading the video again.'
      : code === 3 ? 'The browser could not decode this video. You can try loading it again.'
      : code === 4 ? 'This video format is not supported by your browser.'
      : 'The video could not be loaded. Please try again.'
    setFailure({ message, retryable: code !== 4 })
  }

  function retryMedia() {
    cancelVideoGesture()
    stopBuffering()
    if (media?.kind !== 'video') {
      setAttempt((value) => value + 1)
      return
    }
    setFailure(null)
    setNotice('')
    setPlaying(false)
    loadedRef.current = false
    resumedRef.current = false
    setVideoAttempt((value) => value + 1)
  }

  async function toggleFullscreen() {
    const stage = stageRef.current
    if (!stage || typeof stage.requestFullscreen !== 'function' || !document.fullscreenEnabled) {
      setNotice('Fullscreen isn’t available in this browser.')
      return
    }
    try {
      if (document.fullscreenElement) {
        await document.exitFullscreen()
      } else {
        await stage.requestFullscreen()
        await lockLandscape()
      }
    } catch {
      setNotice('Fullscreen isn’t available in this browser.')
    }
  }

  function onVideoKeyDown(event: KeyboardEvent<HTMLVideoElement>) {
    if (event.target !== event.currentTarget || blocked) return
    if (event.key === ' ' || event.key === 'Spacebar') {
      event.preventDefault()
      togglePlay()
    } else if (event.key === 'ArrowLeft') {
      event.preventDefault()
      seekTo(secondsRef.current - SKIP_SECONDS)
    } else if (event.key === 'ArrowRight') {
      event.preventDefault()
      seekTo(secondsRef.current + SKIP_SECONDS)
    } else if (event.key === 'ArrowUp') {
      event.preventDefault()
      adjustVolume(0.05)
    } else if (event.key === 'ArrowDown') {
      event.preventDefault()
      adjustVolume(-0.05)
    }
  }

  if (!media && !error) {
    return <main className="media-page">
      <div className="loading-state" role="status"><span className="spinner" />Loading your file…</div>
    </main>
  }

  if (!media) {
    return <main className="media-page">
      <div className="error-state" role="alert">
        <h2>This file could not be opened</h2>
        <p>{error || 'Something went wrong. Please try again.'}</p>
        <button type="button" className="button primary" onClick={() => setAttempt((value) => value + 1)}>Try again</button>
        <Link className="button" to="/library">Back to library</Link>
      </div>
    </main>
  }

  const back = <Link className="media-back" to="/library"><ChevronLeft size={18} aria-hidden="true" /><span>Library</span></Link>

  const details = [media.kind === 'video' ? 'Video' : 'Photo', media.format, media.resolution, media.size]
  if (media.kind === 'video') details.push(time(totalDuration))

  const info = <div className="media-info">
    <h1>{media.name}</h1>
    <p>{details.join(' · ')}</p>
  </div>

  const toast = notice ? <p className="toast" role="status">{notice}</p> : null

  if (media.kind === 'image') {
    return <main className="media-page">
      <header className="media-topbar">{back}</header>
      {!failed && <div className="image-stage">
        <img key={attempt} src={media.src} alt={media.name} onError={() => setFailed(true)} />
      </div>}
      {toast}
      {failed && <div className="error-state" role="alert">
        <h2>This image could not be loaded</h2>
        <p>The file could not be opened. Your other files are unaffected.</p>
        <button type="button" className="button primary" onClick={retryMedia}>Try again</button>
      </div>}
      {info}
    </main>
  }

  const sprite = media.sprite
  // Tiles past `count` are padding, so the index is clamped rather than allowed to
  // run into the blank cells at the end of the sheet.
  const spriteIndex = preview && sprite ? Math.min(sprite.count - 1, Math.max(0, Math.floor(preview.time / sprite.interval))) : 0
  const spriteCol = sprite ? spriteIndex % sprite.cols : 0
  const spriteRow = sprite ? Math.floor(spriteIndex / sprite.cols) : 0

  return <main className="media-page video-page">
    <div className={`media-stage ${showControls ? '' : 'controls-hidden'}`} ref={stageRef} style={{ aspectRatio }} onPointerEnter={revealForCursor} onPointerMove={revealForCursor} onPointerLeave={() => { cursorPosition.current = null }} onPointerDownCapture={(event) => { keyboardNavRef.current = false; if (event.target !== videoRef.current) { revealControls(); cancelVideoGesture() } }} onKeyDownCapture={() => { keyboardNavRef.current = true; cancelVideoGesture(); revealControls() }}>
      <video
        key={`${media.id}:${videoAttempt}`}
        ref={videoRef}
        src={source}
        poster={media.image}
        preload="metadata"
        // CORS mode preserves seekable byte ranges when the service worker passes media through.
        crossOrigin="anonymous"
        playsInline
        tabIndex={0}
        aria-label={`${media.name} — video player. Tap to toggle controls.`}
        onClick={(event) => { if (event.detail === 0) { cancelVideoGesture(); toggleControls() } }}
        onPointerDown={startVideoTap}
        onPointerMove={moveVideoTap}
        onPointerUp={finishVideoTap}
        onPointerCancel={cancelVideoGesture}
        onPointerLeave={() => { if (tapStart.current) cancelVideoGesture() }}
        onLostPointerCapture={() => { if (tapStart.current) cancelVideoGesture() }}
        onContextMenu={(event) => event.preventDefault()}
        onKeyDown={onVideoKeyDown}
        onLoadedMetadata={onLoadedMetadata}
        onTimeUpdate={onTimeUpdate}
        onPlay={() => { setPlaying(true); if ((videoRef.current?.readyState ?? 0) < 3) startBuffering() }}
        onPlaying={stopBuffering}
        onWaiting={startBuffering}
        onSeeking={startBuffering}
        onSeeked={() => { if ((videoRef.current?.readyState ?? 0) >= 3) stopBuffering() }}
        onCanPlay={stopBuffering}
        onEmptied={stopBuffering}
        onPause={() => { cancelVideoGesture(); stopBuffering(); setPlaying(false); sendProgress() }}
        onEnded={() => { cancelVideoGesture(); stopBuffering(); setPlaying(false); sendProgress() }}
        onVolumeChange={syncVolume}
        onError={onVideoError}
      />

      {blocked && <div className="player-status" role={processing ? 'status' : 'alert'}>
        <div className="player-status-content">
          {processing && <LoaderCircle size={24} aria-hidden="true" />}
          <h2>{processing ? 'Preparing this video' : 'This video could not be played'}</h2>
          <p>{processing ? 'A browser-compatible version is being prepared. You can leave and come back later.' : backendFailure ? media.playback?.reason : failure?.message}</p>
          <div className="player-status-actions">
            {!backendFailure && failure?.retryable && <button type="button" className="button primary" onClick={retryMedia}>Reload video</button>}
            <Link className="button" to="/library">Back to library</Link>
          </div>
        </div>
      </div>}
      {toast}
      {buffering && !blocked && <div className="player-buffering" role="status" aria-label="Buffering video"><LoaderCircle size={30} aria-hidden="true" /><span className="sr-only">Buffering…</span></div>}
      {speedBoost && <div className="speed-boost" role="status" aria-label="Temporary playback speed 2×">2×</div>}

      <div className="player-controls" ref={controlsRef} inert={!showControls} aria-hidden={!showControls} onFocusCapture={revealControls} onBlurCapture={() => { if (!controlsRef.current?.inert) revealControls() }}>
      <div className="media-overlay-top">
        {back}
        <span className="media-title">{media.name}</span>
        <button type="button" className="icon-button settings-toggle mobile-settings" disabled={blocked} aria-label="Playback settings" aria-haspopup="dialog" aria-expanded={!!settings} onClick={(event) => { if (settings) closeSettings(); else setSettings(event.currentTarget) }}><Settings size={18} aria-hidden="true" /></button>
      </div>

      <div className="center-controls">
        <button type="button" className="skip-control" disabled={blocked} aria-label={`Back ${SKIP_SECONDS} seconds`} onClick={() => seekTo(secondsRef.current - SKIP_SECONDS)}>
          <RotateCcw size={22} aria-hidden="true" />
          <span aria-hidden="true">{SKIP_SECONDS}</span>
        </button>
        <button type="button" className={`play-control${buffering ? ' buffering' : ''}`} disabled={blocked} aria-label={playing ? 'Pause' : 'Play'} onClick={togglePlay}>
          {playing ? <Pause size={26} aria-hidden="true" /> : <Play size={26} aria-hidden="true" />}
        </button>
        <button type="button" className="skip-control" disabled={blocked} aria-label={`Forward ${SKIP_SECONDS} seconds`} onClick={() => seekTo(secondsRef.current + SKIP_SECONDS)}>
          <RotateCw size={22} aria-hidden="true" />
          <span aria-hidden="true">{SKIP_SECONDS}</span>
        </button>
      </div>

      <div className="bottom-controls">
        <div className="timeline">
          <span>{time(current)}</span>
          <input
            type="range"
            min={0}
            max={totalDuration || 0}
            step={1}
            value={Math.min(current, totalDuration || 0)}
            onChange={onSeek}
            // Observational only: no preventDefault, so the native drag and the
            // stage's revealControls keep working exactly as before.
            onInput={(event) => updatePreviewFromValue(event.currentTarget)}
            onPointerMove={(event) => { if (event.pointerType === 'mouse') updatePreview(event.clientX, event.currentTarget) }}
            onPointerDown={(event) => { setSeeking(true); updatePreview(event.clientX, event.currentTarget) }}
            // Also released here, not only on the window: a quick tap can end before the
            // window listener for this drag has been registered.
            onPointerUp={() => { setSeeking(false); setPreview(null) }}
            onPointerCancel={() => { setSeeking(false); setPreview(null) }}
            onPointerLeave={(event) => { if (event.pointerType === 'mouse') setPreview(null) }}
            onBlur={() => setPreview(null)}
            disabled={blocked}
            aria-label="Seek"
            aria-valuetext={`${time(current)} of ${time(totalDuration)}`}
          />
          <time>{time(totalDuration)}</time>
        </div>
        <div className="volume-control">
          <button type="button" className={muted ? 'icon-button muted' : 'icon-button'} disabled={blocked} aria-label={muted ? 'Unmute' : 'Mute'} onClick={toggleMute}>
            {muted ? <VolumeX size={20} aria-hidden="true" /> : <Volume2 size={20} aria-hidden="true" />}
          </button>
          <input type="range" min={0} max={1} step={0.05} value={muted ? 0 : volume} onChange={onVolume} disabled={blocked} aria-label="Volume" />
        </div>
        <button type="button" className="icon-button settings-toggle desktop-settings" disabled={blocked} aria-label="Playback settings" aria-haspopup="dialog" aria-expanded={!!settings} onClick={(event) => { if (settings) closeSettings(); else setSettings(event.currentTarget) }}><Settings size={20} aria-hidden="true" /></button>
        <button type="button" className="icon-button" aria-label={fullscreen ? 'Exit fullscreen' : 'Enter fullscreen'} onClick={toggleFullscreen}>
          {fullscreen ? <Minimize size={20} aria-hidden="true" /> : <Maximize size={20} aria-hidden="true" />}
        </button>
      </div>
      </div>
      {/* Fetched up front so the first hover is not blank, and so a 404 simply turns
          the preview off instead of showing a broken frame. */}
      {sprite && <img className="sr-only" alt="" aria-hidden="true" src={sprite.url} onLoad={() => setSpriteOk(true)} onError={() => setSpriteOk(false)} />}
      {/* Decorative: the accessible time already comes from the slider's
          aria-valuetext, which updates on keyboard changes rather than on every
          pointer move. */}
      {preview && sprite && spriteOk && <div className="seek-preview" aria-hidden="true" style={{ left: preview.left, top: preview.top }}>
        <span className="seek-preview-frame" style={{ backgroundImage: `url(${sprite.url})`, backgroundPosition: `-${spriteCol * PREVIEW_FRAME_W}px -${spriteRow * PREVIEW_FRAME_H}px` }} />
        <span className="seek-preview-time">{time(preview.time)}</span>
      </div>}
      {skipFeedback && <div className={`skip-feedback skip-feedback-${skipFeedback.direction}`} role="status" aria-label={`${skipFeedback.direction === 'forward' ? 'Forward' : 'Back'} ${skipFeedback.seconds} seconds`}>
        <span aria-hidden="true">{skipFeedback.direction === 'forward' ? `+${skipFeedback.seconds} →` : `← −${skipFeedback.seconds}`}</span>
      </div>}
      {settings && <PlayerSettings rate={rate} opener={settings} onRate={onRate} onClose={closeSettings} />}
    </div>

    <div className="video-details">
    {info}
    {media.progress > 0 && <p className="resume-badge">Resume at {time(media.progress)}</p>}
    </div>
  </main>
}
