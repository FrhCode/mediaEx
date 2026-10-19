import assert from 'node:assert/strict'
import { mkdir } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const require = createRequire(new URL('../frontend/package.json', import.meta.url))
const { chromium, expect } = require('@playwright/test')
const baseURL = process.env.TEST_BASE_URL ?? 'http://127.0.0.1:8080'
const adminName = process.env.TEST_ADMIN_NAME
const adminPassword = process.env.TEST_ADMIN_PASSWORD
assert(adminName && adminPassword, 'Set TEST_ADMIN_NAME and TEST_ADMIN_PASSWORD')
const artifacts = fileURLToPath(new URL('../frontend/test-results/backend/', import.meta.url))
await mkdir(artifacts, { recursive: true })
const browser = await chromium.launch({ channel: 'chrome', headless: true })
const adminContext = await browser.newContext({ baseURL, viewport: { width: 1440, height: 1000 } })
const userContext = await browser.newContext({ baseURL, viewport: { width: 390, height: 844 } })
const page = await adminContext.newPage()
const userPage = await userContext.newPage()
const errors = []
page.on('pageerror', error => errors.push(error.message))
userPage.on('pageerror', error => errors.push(error.message))
const username = 'browser-' + Date.now()
const password = 'Browser-test-password-' + Date.now()
let createdID

async function api(context, path, method = 'GET', data, status = 200) {
  const response = await context.request.fetch('/api' + path, { method, data, headers: { Origin: baseURL } })
  assert.equal(response.status(), status, `${method} ${path}: ${await response.text()}`)
  return response.json()
}

async function rowAction(row, label, confirmation) {
  await row.getByRole('button', { name: label, exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: confirmation, exact: true }).click()
  await expect(dialog).not.toBeVisible()
}

async function login(target, name, secret) {
  await target.goto('/library')
  await expect(target).toHaveURL(/\/login/)
  await target.getByLabel('Username', { exact: true }).fill(name)
  await target.getByLabel('Password', { exact: true }).fill(secret)
  await target.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(target.getByLabel('Search your media', { exact: true })).toBeVisible()
}

try {
  await login(page, adminName, adminPassword)
  await expect(page.getByRole('button', { name: 'Open testing tools' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Open account' }).click()
  await page.getByRole('dialog').getByRole('link', { name: 'Manage accounts', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Accounts', exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Create account', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Username', { exact: true }).fill(username)
  await dialog.getByLabel('Display name', { exact: true }).fill('Browser Test User')
  await dialog.getByLabel('Password', { exact: true }).fill(password)
  await dialog.getByRole('button', { name: 'Create account', exact: true }).click()
  await expect(dialog).not.toBeVisible()
  await expect(page.getByText(username, { exact: true })).toBeVisible()
  const users = await api(adminContext, '/admin/users')
  createdID = users.users.find(user => user.username === username).id
  await page.screenshot({ path: artifacts + 'accounts-desktop.png', fullPage: true })

  await login(userPage, username, password)
  await expect(userPage.getByRole('link', { name: 'Manage accounts', exact: true })).toHaveCount(0)
  assert(await userPage.evaluate(() => document.documentElement.scrollWidth <= innerWidth))
  await userPage.screenshot({ path: artifacts + 'library-mobile.png', fullPage: true })
  await api(userContext, '/admin/users', 'GET', undefined, 403)
  const library = await api(userContext, '/library')
  const video = library.items.find(item => item.kind === 'video')
  const image = library.items.find(item => item.kind === 'image')
  assert(video && image, 'Seed both video and image fixtures')
  await userPage.getByLabel('Search your media', { exact: true }).fill('definitely-no-match-xyz')
  await expect(userPage.getByRole('heading', { name: 'No matching moments' })).toBeVisible()
  await userPage.getByRole('button', { name: 'Clear search', exact: true }).first().click()
  await userPage.goto('/media/' + image.id)
  await expect(userPage.locator('.image-stage img')).toBeVisible()
  await expect.poll(() => userPage.locator('.image-stage img').evaluate(element => element.naturalWidth)).toBeGreaterThan(0)
  const folder = await api(userContext, '/library?folder=' + video.folderId)
  if (folder.breadcrumbs?.length > 1) {
    await userPage.goto('/library')
    await userPage.getByRole('button', { name: 'Open account' }).click()
    await userPage.getByLabel('Mode tampilan', { exact: true }).selectOption('explorer')
    await userPage.keyboard.press('Escape')
    await userPage.goto('/library?folder=' + video.folderId)
    await expect(userPage.getByRole('navigation', { name: 'Breadcrumb', exact: true })).toContainText(folder.folder.name)
  }
  await userPage.goto('/media/' + video.id)
  const player = userPage.locator('video')
  await expect.poll(() => player.evaluate(element => element.readyState)).toBeGreaterThanOrEqual(1)
  await userPage.getByRole('button', { name: 'Play', exact: true }).click()
  await expect.poll(() => player.evaluate(element => element.currentTime)).toBeGreaterThan(0.2)
  await userPage.getByRole('button', { name: 'Pause', exact: true }).click()
  await userPage.getByRole('slider', { name: 'Seek', exact: true }).fill('12')
  await expect.poll(() => player.evaluate(element => element.currentTime)).toBeCloseTo(12, 0)
  await userPage.getByRole('link', { name: 'Library', exact: true }).click()
  await expect.poll(async () => (await api(userContext, '/items/' + video.id)).progress).toBe(12)
  await userPage.goto('/media/' + video.id)
  await expect.poll(() => player.evaluate(element => element.currentTime)).toBeCloseTo(12, 0)
  await userPage.reload()
  await expect.poll(() => player.evaluate(element => element.currentTime)).toBeCloseTo(12, 0)
  await userPage.screenshot({ path: artifacts + 'player-mobile.png', fullPage: true })
  await expect.poll(async () => (await api(adminContext, '/items/' + video.id)).progress).not.toBe(12)

  const row = page.locator('article').filter({ hasText: username })
  await rowAction(row, 'Disable', 'Disable account')
  await expect(row).toContainText('Disabled')
  await api(userContext, '/library', 'GET', undefined, 401)
  await userPage.getByRole('link', { name: 'Library', exact: true }).click()
  await expect(userPage).toHaveURL(/\/login/)
  const content = await userContext.request.get(video.src)
  assert.equal(content.status(), 401)
  await rowAction(row, 'Enable', 'Enable account')
  await expect(row).toContainText('Active')
  await login(userPage, username, password)

  await page.setViewportSize({ width: 390, height: 844 })
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth))
  await page.screenshot({ path: artifacts + 'accounts-mobile.png', fullPage: true })
  await rowAction(row, 'Delete', 'Delete account')
  await expect(page.getByText(username, { exact: true })).toHaveCount(0)
  createdID = null
  await api(userContext, '/library', 'GET', undefined, 401)
  const session = await api(adminContext, '/session')
  await api(adminContext, '/admin/users/' + session.id, 'PATCH', { active: false }, 409)
  assert.deepEqual(errors, [])
  console.log('Real browser E2E passed: admin create/disable/enable/delete, mobile, image, search, recursive navigation, video playback/seek/resume, isolation and access revocation.')
} finally {
  if (createdID) await api(adminContext, '/admin/users/' + createdID, 'DELETE').catch(() => {})
  await browser.close()
}
