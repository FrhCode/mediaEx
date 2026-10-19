import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { startLockLifecycle } from './appLock'
import { startMediaBridge } from './mediaBridge'

async function start() {
  if (import.meta.env.VITE_ENABLE_MOCKS === 'true') {
    const { worker } = await import('./mocks/browser')
    await worker.start({ quiet: true, onUnhandledFrame: 'bypass' })
  } else {
    startMediaBridge()
  }
  startLockLifecycle()
  createRoot(document.getElementById('root')!).render(<StrictMode><App /></StrictMode>)
}

start().catch(() => {
  const root = document.getElementById('root')!
  root.textContent = 'Media Explorer could not start. Please reload the page.'
})
