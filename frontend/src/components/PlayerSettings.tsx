import { useEffect, useId, useRef, useState } from 'react'
import { Check, ChevronLeft, ChevronRight, X } from 'lucide-react'

const SPEEDS = [0.5, 0.75, 1, 1.25, 1.5, 2]

export default function PlayerSettings({ rate, opener, onRate, onClose }: { rate: number; opener: HTMLButtonElement; onRate: (value: number) => void; onClose: () => void }) {
  const [screen, setScreen] = useState<'main' | 'speed'>('main')
  const ref = useRef<HTMLDialogElement>(null)
  const contentRef = useRef<HTMLDivElement>(null)
  const swipeStart = useRef<number | null>(null)
  const titleId = useId()
  const mobile = window.matchMedia('(max-width: 700px)').matches

  useEffect(() => {
    const panel = ref.current!
    const viewport = window.matchMedia('(max-width: 700px)')
    function position() {
      const bounds = opener.getBoundingClientRect()
      panel.style.left = `${Math.max(12, Math.min(bounds.right - 280, window.innerWidth - 292))}px`
      panel.style.top = `${bounds.top - 8}px`
      panel.style.maxHeight = `${Math.max(100, bounds.top - 20)}px`
    }
    if (mobile) panel.showModal()
    else { position(); panel.showPopover() }
    function outside(event: PointerEvent) {
      const target = event.target as Element
      if (!mobile && !panel.contains(target) && !target.closest('.settings-toggle')) onClose()
    }
    function escape(event: globalThis.KeyboardEvent) {
      if (event.key === 'Escape' && !mobile) { event.preventDefault(); event.stopPropagation(); onClose() }
    }
    document.addEventListener('pointerdown', outside)
    document.addEventListener('keydown', escape, true)
    viewport.addEventListener('change', onClose)
    window.addEventListener('resize', position)
    window.addEventListener('scroll', position, true)
    return () => {
      if (mobile) panel.close()
      else panel.hidePopover()
      document.removeEventListener('pointerdown', outside)
      document.removeEventListener('keydown', escape, true)
      viewport.removeEventListener('change', onClose)
      window.removeEventListener('resize', position)
      window.removeEventListener('scroll', position, true)
      if (opener.isConnected) opener.focus({ preventScroll: true })
    }
  }, [mobile, opener, onClose])

  useEffect(() => {
    contentRef.current?.querySelector<HTMLElement>(screen === 'speed' ? '[aria-checked="true"]' : '.speed-menu-entry')?.focus({ preventScroll: true })
  }, [screen])

  return <dialog ref={ref} popover={mobile ? undefined : 'manual'} className={`playback-menu ${mobile ? 'playback-sheet' : 'playback-popup'}`} aria-labelledby={titleId} onCancel={(event) => { event.preventDefault(); onClose() }} onClick={(event) => { if (event.target === event.currentTarget) { const bounds = event.currentTarget.getBoundingClientRect(); if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) onClose() } }}>
    <div className="playback-menu-header" onPointerDown={(event) => { if (mobile && event.pointerType === 'touch' && !(event.target as Element).closest('button')) { swipeStart.current = event.clientY; event.currentTarget.setPointerCapture(event.pointerId) } }} onPointerUp={(event) => { if (swipeStart.current !== null && event.clientY - swipeStart.current > 60) onClose(); swipeStart.current = null }} onPointerCancel={() => { swipeStart.current = null }}>
      <span className="sheet-handle" aria-hidden="true" />
      {screen === 'speed' && <button type="button" className="icon-button" aria-label="Back to playback settings" onClick={() => setScreen('main')}><ChevronLeft size={20} /></button>}
      <h2 id={titleId}>{screen === 'main' ? 'Playback settings' : 'Playback speed'}</h2>
      <button type="button" className="icon-button" aria-label="Close playback settings" onClick={onClose}><X size={19} /></button>
    </div>
    <div ref={contentRef} className="playback-menu-content">
      {screen === 'main' ? <button type="button" className="speed-menu-entry" onClick={() => setScreen('speed')}><span>Playback speed</span><span>{rate === 1 ? 'Normal' : `${rate}×`}</span><ChevronRight size={17} /></button> : <div role="group" aria-label="Playback speed">{SPEEDS.map((value) => <button type="button" role="radio" aria-checked={value === rate} className="speed-option" key={value} onClick={() => { onRate(value); setScreen('main') }}><span className="speed-check">{value === rate && <Check size={19} aria-hidden="true" />}</span>{value === 1 ? 'Normal' : `${value}×`}</button>)}</div>}
    </div>
  </dialog>
}
