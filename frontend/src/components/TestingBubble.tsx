import { useState } from 'react'
import { Check, FlaskConical } from 'lucide-react'
import type { Session } from '../api/client'
import Dialog from './Dialog'

export default function TestingBubble({ session, scenario, onScenario }: { session: Session; scenario: string; onScenario: (value: string) => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function applyScenario(value: string) {
    setBusy(true)
    setError('')
    try {
      await onScenario(value)
      setOpen(false)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Unable to change the testing scenario.')
    } finally {
      setBusy(false)
    }
  }

  return <aside className="testing-tools" aria-label="Testing tools">
    <button className="testing-bubble" aria-label="Open testing tools" aria-haspopup="dialog" aria-expanded={open} onClick={() => { setError(''); setOpen(true) }}><FlaskConical size={23} aria-hidden="true" /></button>
    {open && <Dialog title="Testing tools" className="testing-panel" onClose={() => setOpen(false)}>
      <p className="muted">Frontend preview only. All API requests use mock data in this browser; no backend or real authentication is connected.</p>
      <section className="testing-section" aria-label="Demo credentials">
        <h3>Demo sign-in</h3>
        <dl className="detail-list"><div><dt>Username</dt><dd><code>demo</code></dd></div><div><dt>Password</dt><dd><code>demo123</code></dd></div></dl>
        <p className="small-note">Use these sample credentials only. Do not enter a real password. Session and playback progress stay in this browser tab.</p>
      </section>
      <section className="testing-section" aria-label="Library scenarios">
        <h3>Library scenarios</h3>
        <p className="small-note">{session ? 'Apply a state to the library. On other pages, it takes effect when you return to the library.' : 'Sign in to test loading, empty, and error states.'}</p>
        <div className="scenario-list">{[['normal', 'Normal library', 'Browse or restore the complete collection.'], ['slow', 'Slow connection', 'Show loading states for two seconds.'], ['empty', 'Empty library', 'See how a fresh collection looks.'], ['error', 'Connection error', 'Fail the next library load, then test retry.'], ['converting', 'Preparing video', 'Drone Footage becomes ready after eight seconds.'], ['unplayable', 'Unsupported video', 'Drone Footage has no supported video stream.']].map(([value, title, subtitle]) => <button type="button" disabled={!session || busy} key={value} onClick={() => void applyScenario(value)} className="scenario-button" aria-pressed={scenario === value}><div><strong>{title}</strong><span>{subtitle}</span></div>{scenario === value && <Check size={18} aria-hidden="true" />}</button>)}</div>
        {error && <p className="form-error" role="alert">{error}</p>}
      </section>
      <section className="testing-section" aria-label="Sample media information">
        <h3>Sample media</h3>
        <p className="small-note">Video files share one local sample clip with no audio track. Playback and resume are real; casting and server storage are not connected.</p>
        <p className="small-note">Media credits: beach photo by Vyacheslav Argenberg (CC BY 4.0); island photo by Matheenfaiz (CC BY-SA 4.0); cropped versions retain their licenses. Other photos: Julius_Silver and W.carter (CC0). Video: Mixkit Free License. <a className="text-button" href="/media/credits.json" target="_blank" rel="noreferrer">Sources and license details</a></p>
      </section>
    </Dialog>}
  </aside>
}
