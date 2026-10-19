import assert from 'node:assert/strict'
import { createRequire } from 'node:module'

const require = createRequire(new URL('../frontend/package.json', import.meta.url))
const { chromium } = require('@playwright/test')
const baseURL = process.env.TEST_BASE_URL ?? 'http://localhost:5189'
const username = process.env.TEST_ADMIN_NAME
const password = process.env.TEST_ADMIN_PASSWORD
assert(username && password, 'Set TEST_ADMIN_NAME and TEST_ADMIN_PASSWORD')
const browser = await chromium.launch({ channel: 'chrome', headless: true })
const context = await browser.newContext({ baseURL })
const page = await context.newPage()
const cdp = await context.newCDPSession(page)
await cdp.send('WebAuthn.enable')
const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
  options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
})

async function api(path, body, status = 200) {
  const response = await context.request.fetch('/api' + path, {
    method: body === undefined ? 'GET' : 'POST', data: body, headers: { Origin: baseURL },
  })
  assert.equal(response.status(), status, `${path}: ${await response.text()}`)
  const result = await response.json()
  assert(!result || !Object.hasOwn(result, 'grant'), 'Raw unlock secret must not be returned to JavaScript')
  return result
}

async function credential(publicKey, registration) {
  return page.evaluate(async ({ publicKey, registration }) => {
    const decode = value => Uint8Array.from(atob(value.replace(/-/g, '+').replace(/_/g, '/')), char => char.charCodeAt(0))
    const encode = value => btoa(String.fromCharCode(...new Uint8Array(value))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
    const options = { ...publicKey, challenge: decode(publicKey.challenge) }
    if (registration) {
      options.user = { ...options.user, id: decode(options.user.id) }
      options.excludeCredentials = options.excludeCredentials?.map(item => ({ ...item, id: decode(item.id) }))
    } else {
      options.allowCredentials = options.allowCredentials?.map(item => ({ ...item, id: decode(item.id) }))
    }
    const result = await navigator.credentials[registration ? 'create' : 'get']({ publicKey: options })
    if (typeof result.toJSON === 'function') return result.toJSON()
    const response = result.response
    return {
      id: result.id, rawId: encode(result.rawId), type: result.type,
      authenticatorAttachment: result.authenticatorAttachment,
      clientExtensionResults: result.getClientExtensionResults(),
      response: registration
        ? { clientDataJSON: encode(response.clientDataJSON), attestationObject: encode(response.attestationObject), transports: response.getTransports?.() ?? [] }
        : { clientDataJSON: encode(response.clientDataJSON), authenticatorData: encode(response.authenticatorData), signature: encode(response.signature), userHandle: response.userHandle ? encode(response.userHandle) : null },
    }
  }, { publicKey, registration })
}

async function unlockCookie() {
  const cookie = (await context.cookies(baseURL + '/api/lock/status')).find(cookie => cookie.name === 'mediaex_unlock')
  assert(cookie, 'Unlock cookie must be set')
  assert(cookie.httpOnly)
  assert.equal(cookie.path, '/api')
  assert.equal(cookie.sameSite, 'Lax')
  assert(cookie.expires * 1000 <= Date.now() + 61000)
  return cookie
}

let loginOnly
try {
  await page.goto('/login')
  const session = await api('/login', { username, password })
  assert.equal(session.lockEnabled, false)
  const library = await api('/library')
  const video = library.items.find(item => item.kind === 'video')
  assert(video, 'Seed a video before running this test')
  const loginCookies = await context.cookies()
  loginOnly = await browser.newContext({ baseURL })
  await loginOnly.addCookies(loginCookies)
  const options = await api('/lock/register/options', { password })
  const registration = await credential(options.publicKey, true)
  const enabled = await api('/lock/register/finish', { ceremonyId: options.ceremonyId, credential: registration })
  assert(enabled.expiresAt)
  assert.equal(enabled.session.lockEnabled, true)
  const cookie = await unlockCookie()
  assert(!(await page.evaluate(() => document.cookie)).includes(cookie.value))
  const state = await api('/session')
  assert.equal(state.lockEnabled, true)
  assert.equal(state.hasPasskey, true)
  for (const path of ['/api/library', '/api/admin/users', video.src, video.image, video.sprite?.url].filter(Boolean)) {
    const response = await loginOnly.request.get(path)
    assert.equal(response.status(), 423, path)
    assert.match(response.headers()['cache-control'], /no-store/)
  }
  assert.equal((await loginOnly.request.get('/api/library', { headers: { 'X-Mediaex-Grant': cookie.value } })).status(), 423)
  await api('/library')
  const status = await api('/lock/status')
  assert.equal(status.grantActive, true)
  assert.equal(status.expiresAt, enabled.expiresAt)
  assert.equal((await api('/lock/status')).expiresAt, status.expiresAt)
  const head = await context.request.head(video.src)
  assert.equal(head.status(), 200)
  assert.equal((await head.body()).length, 0)
  const invalidRange = await context.request.get(video.src, { headers: { Range: 'bytes=999999999999-' } })
  assert.equal(invalidRange.status(), 416)
  const range = await context.request.get(video.src, { headers: { Range: 'bytes=0-99' } })
  assert.equal(range.status(), 206)
  assert.equal((await range.body()).length, 100)
  await api('/lock/renew', {})
  assert.equal((await unlockCookie()).value, cookie.value)
  await api('/lock/lock', { progress: { itemId: video.id, seconds: 7 } })
  await api('/library', undefined, 423)
  await api('/lock/renew', {}, 423)
  await api('/lock/password', { password: 'not-the-right-password' }, 403)
  assert.equal((await api('/session')).id, session.id)
  const unlockOptions = await api('/lock/unlock/options', {})
  const assertion = await credential(unlockOptions.publicKey, false)
  const unlocked = await api('/lock/unlock/finish', { ceremonyId: unlockOptions.ceremonyId, credential: assertion })
  assert(unlocked.expiresAt)
  assert.notEqual((await unlockCookie()).value, cookie.value)
  assert.equal((await api('/items/' + video.id)).progress, 7)
  await api('/lock/unlock/finish', { ceremonyId: unlockOptions.ceremonyId, credential: assertion }, 403)
  await api('/lock/password', { password })
  await api('/library')
  await api('/lock/disable', { password })
  assert.equal((await api('/session')).lockEnabled, false)
  await api('/library')
  const enableOptions = await api('/lock/enable/options', {})
  const enableAssertion = await credential(enableOptions.publicKey, false)
  const reenabled = await api('/lock/enable/finish', { ceremonyId: enableOptions.ceremonyId, credential: enableAssertion })
  assert.equal(reenabled.session.lockEnabled, true)
  assert.equal((await loginOnly.request.get('/api/library')).status(), 423)
  await api('/library')
  await cdp.send('WebAuthn.setUserVerified', { authenticatorId, isUserVerified: false })
  const noUV = await api('/lock/unlock/options', {})
  const rejected = await page.evaluate(async publicKey => {
    const decode = value => Uint8Array.from(atob(value.replace(/-/g, '+').replace(/_/g, '/')), char => char.charCodeAt(0))
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), 1500)
    try {
      await navigator.credentials.get({ signal: controller.signal, publicKey: { ...publicKey, timeout: 1000, challenge: decode(publicKey.challenge), allowCredentials: publicKey.allowCredentials.map(item => ({ ...item, id: decode(item.id) })) } })
      return false
    } catch { return true } finally { clearTimeout(timer) }
  }, noUV.publicKey)
  assert(rejected, 'Required user verification must not silently succeed without verification')
  await cdp.send('WebAuthn.setUserVerified', { authenticatorId, isUserVerified: true })
  await api('/lock/disable', { password })
  await api('/logout', {})
  await api('/library', undefined, 401)
  console.log('Real WebAuthn cookie E2E passed: HttpOnly cookie, no raw secret JSON/header bypass, 60s lease, status no-renew, content gate, signed assertion, replay rejection, user verification, existing-passkey activation, password fallback, renewal/revocation, progress, disable, logout, Range/HEAD/416.')
} finally {
  await loginOnly?.close()
  await context.close()
  await browser.close()
}
