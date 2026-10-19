import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { mkdir } from 'node:fs/promises'
const require = createRequire(new URL('../frontend/package.json', import.meta.url))
const { chromium, expect: baseExpect } = require('@playwright/test')
const expect = baseExpect.configure({ timeout: 15000 })
const baseURL = process.env.TEST_BASE_URL ?? 'http://localhost:5189'
const username = process.env.TEST_ADMIN_NAME
const password = process.env.TEST_ADMIN_PASSWORD
assert(username && password)
const browser = await chromium.launch({ channel: 'chrome', headless: true })
const context = await browser.newContext({ baseURL, viewport: { width: 390, height: 844 } })
const page = await context.newPage()
const cdp = await context.newCDPSession(page)
const errors = []
const renewals = []
page.on('pageerror', error => errors.push(error.message))
page.on('response', response => {
  if (response.url().endsWith('/api/lock/renew')) renewals.push({ at: Date.now(), status: response.status() })
  if (response.status() >= 400) console.log('HTTP', response.status(), response.url())
})
await cdp.send('WebAuthn.enable')
await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } })
const artifacts = fileURLToPath(new URL('../frontend/test-results/app-lock/', import.meta.url))
await mkdir(artifacts, { recursive: true })
const locked = page.getByRole('heading', { name: 'This session is locked', exact: true })
const search = page.getByLabel('Search your media', { exact: true })
async function unlockPassword(secret = password) {
  await page.getByLabel('Account password', { exact: true }).fill(secret)
  await page.getByRole('button', { name: 'Unlock', exact: true }).click()
}
async function passkeyUnlock() {
  await page.getByRole('button', { name: 'Unlock with passkey', exact: true }).click()
  await expect(locked).not.toBeVisible()
}
async function account() { await page.getByRole('button', { name: 'Open account', exact: true }).click() }
async function visibility(state) {
  await page.evaluate(state => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: state })
    document.dispatchEvent(new Event('visibilitychange'))
  }, state)
}
async function post(path, data = {}) {
  return context.request.post('/api' + path, { headers: { Origin: baseURL }, data })
}
async function status() { return (await context.request.get('/api/lock/status')).json() }
async function bounded(promise) {
  let timer
  try { return await Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('Timed out waiting for held API response')), 20000) })]) }
  finally { clearTimeout(timer) }
}
try {
  await page.goto('/library')
  await page.getByLabel('Username', { exact: true }).fill(username)
  await page.getByLabel('Password', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(search).toBeVisible()
  const library = await (await context.request.get('/api/library')).json()
  const video = library.items.find(item => item.kind === 'video')
  const image = library.items.find(item => item.kind === 'image')
  assert(video && image)
  await account()
  const existingPasskey = await page.getByLabel('Add another passkey', { exact: true }).isVisible()
  await page.getByLabel(existingPasskey ? 'Add another passkey' : 'Create a passkey', { exact: true }).fill(password)
  const registered = page.waitForResponse(response => response.url().endsWith('/api/lock/register/finish') && response.ok())
  await page.getByRole('button', { name: existingPasskey ? 'Add passkey' : 'Create passkey and turn on', exact: true }).click()
  await registered
  await expect(search).toBeVisible()
  await account()
  await expect(page.getByText('On for this session', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Lock now', exact: true }).click()
  await expect(locked).toBeVisible()
  assert.equal((await context.request.get('/api/library')).status(), 423)
  assert.equal((await context.request.get(video.src)).status(), 423)
  await page.keyboard.press('Escape')
  await expect(locked).toBeVisible()
  await unlockPassword('wrong-password')
  await expect(page.getByRole('alert')).toContainText('incorrect')
  await passkeyUnlock()
  await expect(search).toBeVisible()
  await expect.poll(() => page.locator('.thumbnail img').first().evaluate(img => img.naturalWidth)).toBeGreaterThan(0)
  const cookie = (await context.cookies(baseURL + '/api/library')).find(cookie => cookie.name === 'mediaex_unlock')
  assert(cookie?.httpOnly)
  assert.equal(cookie.path, '/api')
  assert(!(await page.evaluate(() => document.cookie)).includes(cookie.value))
  await page.screenshot({ path: artifacts + 'library-unlocked.png', fullPage: true })
  await page.reload()
  await expect(search).toBeVisible()
  await expect(locked).not.toBeVisible()
  await page.getByRole('link').filter({ hasText: image.name }).first().click()
  await expect.poll(() => page.locator('.image-stage img').evaluate(img => img.naturalWidth)).toBeGreaterThan(0)
  await page.getByRole('link', { name: 'Library', exact: true }).click()
  await page.getByRole('link').filter({ hasText: video.name }).first().click()
  const player = page.locator('video')
  await expect.poll(() => player.evaluate(video => video.readyState)).toBeGreaterThanOrEqual(1)
  await page.getByRole('slider', { name: 'Seek', exact: true }).fill('8')
  await player.evaluate(video => video.pause())
  await expect.poll(() => player.evaluate(video => video.currentTime)).toBeCloseTo(8, 0)
  const progressSaved = page.waitForResponse(response => response.url().endsWith('/progress') && response.request().method() === 'PUT' && response.ok())
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
    document.dispatchEvent(new Event('visibilitychange'))
    window.dispatchEvent(new PageTransitionEvent('pagehide'))
  })
  await expect(player).toHaveCount(0)
  await progressSaved
  assert.equal((await status()).grantActive, true)
  await visibility('visible')
  await expect.poll(() => player.evaluate(video => video.currentTime)).toBeCloseTo(8, 0)
  assert(await player.evaluate(video => video.paused), 'Return from background must resume paused')
  await player.evaluate(async video => { video.loop = true; await video.play() })
  for (let lease = 0; lease < 5; lease++) {
    await page.waitForResponse(response => response.url().endsWith('/api/lock/renew') && response.ok(), { timeout: 25000 })
    await expect(player).toBeVisible()
    await expect(locked).not.toBeVisible()
  }
  assert(renewals.filter(item => item.status === 200).length >= 5)
  await player.evaluate(video => video.pause())
  await cdp.send('ServiceWorker.enable')
  await cdp.send('ServiceWorker.stopAllWorkers')
  const legacyReply = await page.evaluate(async () => {
    const channel = new MessageChannel()
    const reply = new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('No legacy worker reply')), 1500)
      channel.port1.onmessage = event => { clearTimeout(timer); resolve(event.data) }
    })
    navigator.serviceWorker.dispatchEvent(new MessageEvent('message', { source: navigator.serviceWorker.controller, data: { type: 'mediaex:grant', nonce: 'legacy-test' }, ports: [channel.port2] }))
    return reply
  })
  assert.deepEqual(legacyReply, { nonce: 'legacy-test', grant: '' })
  await page.getByRole('link', { name: 'Library', exact: true }).click()
  await page.getByRole('link').filter({ hasText: image.name }).first().click()
  await expect.poll(() => page.locator('.image-stage img').evaluate(img => img.naturalWidth)).toBeGreaterThan(0)
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })))
  await expect(page.locator('.image-stage img')).toBeVisible()
  await expect(locked).not.toBeVisible()
  await visibility('hidden')
  const almostExpired = (await status()).expiresAt * 1000
  await page.waitForTimeout(Math.max(0, almostExpired - Date.now() - 5000))
  await visibility('visible')
  await expect(page.locator('.image-stage img')).toBeVisible()
  await page.waitForResponse(response => response.url().endsWith('/api/lock/renew') && response.ok(), { timeout: 25000 })
  await expect(locked).not.toBeVisible()
  await visibility('hidden')
  const other = await context.newPage()
  await other.goto('/library')
  await expect(other.getByLabel('Search your media', { exact: true })).toBeVisible()
  await other.waitForResponse(response => response.url().endsWith('/api/lock/renew') && response.ok(), { timeout: 25000 })
  assert.equal((await other.request.get(video.src)).status(), 200)
  await other.getByRole('button', { name: 'Open account', exact: true }).click()
  await other.getByRole('button', { name: 'Lock now', exact: true }).click()
  await expect(other.getByRole('heading', { name: 'This session is locked', exact: true })).toBeVisible()
  assert.equal((await context.request.get('/api/library')).status(), 423)
  await other.close()
  await page.bringToFront()
  await visibility('visible')
  await expect(locked).toBeVisible()
  await passkeyUnlock()
  await expect(page.locator('.image-stage img')).toBeVisible()
  await visibility('hidden')
  const hiddenRenewals = renewals.length
  const leaseEnd = (await status()).expiresAt * 1000
  await page.waitForTimeout(Math.max(0, leaseEnd - Date.now()) + 1500)
  assert.equal(renewals.length, hiddenRenewals, 'Hidden page must stop heartbeats')
  assert.equal((await status()).grantActive, false)
  assert.equal((await post('/lock/renew')).status(), 423)
  await visibility('visible')
  await expect(locked).toBeVisible()
  await page.screenshot({ path: artifacts + 'locked-mobile.png', fullPage: true })
  let releasePassword
  const passwordHeld = new Promise(resolve => { releasePassword = resolve })
  let passwordReady
  const responseReady = new Promise(resolve => { passwordReady = resolve })
  await page.route('**/api/lock/password', async route => {
    const response = await route.fetch()
    passwordReady()
    await passwordHeld
    await route.fulfill({ response })
  })
  await unlockPassword()
  await bounded(responseReady)
  await visibility('hidden')
  releasePassword()
  await expect(page.locator('.image-stage img')).toHaveCount(0)
  await page.unroute('**/api/lock/password')
  await visibility('visible')
  await expect(page.locator('.image-stage img')).toBeVisible()
  await expect(locked).not.toBeVisible()
  await page.getByRole('link', { name: 'Library', exact: true }).click()
  await account()
  await page.getByLabel('Turn off the session lock', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'Turn off', exact: true }).click()
  await expect(page.getByText('Off', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Close dialog', exact: true }).click()
  await account()
  const reenabled = page.waitForResponse(response => response.url().endsWith('/api/lock/enable/finish') && response.ok())
  await page.getByRole('button', { name: 'Use an existing passkey to turn on', exact: true }).click()
  await reenabled
  await expect(search).toBeVisible()
  await account()
  let releaseRenew
  const renewHeld = new Promise(resolve => { releaseRenew = resolve })
  let renewalReady
  const renewalResponse = new Promise(resolve => { renewalReady = resolve })
  await page.route('**/api/lock/renew', async route => {
    const response = await route.fetch()
    renewalReady()
    await renewHeld
    await route.fulfill({ response })
  }, { times: 1 })
  await bounded(renewalResponse)
  const lockRequest = page.waitForRequest(request => request.url().endsWith('/api/lock/lock'))
  await page.getByRole('button', { name: 'Lock now', exact: true }).click()
  await expect(page.getByText('Locking this session…', { exact: true })).toBeVisible()
  releaseRenew()
  await lockRequest
  await expect(locked).toBeVisible()
  await passkeyUnlock()
  await expect(search).toBeVisible()
  assert.equal((await status()).grantActive, true)
  await account()
  await page.getByLabel('Turn off the session lock', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'Turn off', exact: true }).click()
  await expect(page.getByText('Off', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Close dialog', exact: true }).click()
  await page.reload()
  await expect(search).toBeVisible()
  const persistence = await page.evaluate(async () => ({ local: { ...localStorage }, session: { ...sessionStorage }, caches: await caches.keys() }))
  assert(!JSON.stringify(persistence).includes(cookie.value))
  assert.equal(persistence.caches.length, 0)
  assert(!page.url().includes(cookie.value))
  const manifest = await (await context.request.get('/manifest.webmanifest')).json()
  assert.equal(manifest.display, 'standalone')
  for (const icon of manifest.icons) assert.equal((await context.request.get(icon.src)).status(), 200)
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth))
  await page.goto('/admin/users')
  await page.getByRole('button', { name: 'Sign out', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible()
  assert.equal(await (await context.request.get('/api/session')).json(), null)
  await page.reload()
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible()
  assert.deepEqual(errors, [])
  console.log('Sliding app lock UI passed: real passkey/password, HttpOnly cookie, refresh/tab reuse, native media, background pause/progress, five foreground renewals, worker restart, bfcache event, shared manual revoke, hidden expiry/no heartbeat, stale response concealment, disable, logout, manifest/icons, mobile layout.')
} finally {
  await page.screenshot({ path: artifacts + 'final.png', fullPage: true }).catch(() => undefined)
  await context.close()
  await browser.close()
}
