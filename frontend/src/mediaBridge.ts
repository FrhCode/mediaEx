export function startMediaBridge() {
  if (!('serviceWorker' in navigator) || !window.isSecureContext) return
  // Reply without secrets while an older worker is still controlling the page.
  navigator.serviceWorker.addEventListener('message', event => {
    if (event.source !== navigator.serviceWorker.controller || event.data?.type !== 'mediaex:grant' || typeof event.data.nonce !== 'string' || !event.ports[0]) return
    event.ports[0].postMessage({ nonce: event.data.nonce, grant: '' })
    event.ports[0].close()
  })
  void navigator.serviceWorker.register('/sw.js', { scope: '/', updateViaCache: 'none' }).catch(() => undefined)
}
