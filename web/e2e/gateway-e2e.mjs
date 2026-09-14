import assert from 'node:assert/strict'
import { chromium } from 'playwright-core'

// Launched by the existing Go gateway integration test. No mocked HTTP or SSE.
const browser = await chromium.launch({ headless: true, ...(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : {}) })
try {
  const page = await browser.newPage()
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  await page.goto(process.env.WEBUI_URL)
  await page.waitForSelector('#connection-status:text-is("Connected")')
  await page.waitForSelector('.message-body:has-text("authoritative manager reply")')
  assert.equal((await page.locator('.message.user .user-text').innerText()).trim(), 'hello')
  assert.equal(await page.locator('.message.user .message-meta').innerText(), 'Sent')
  await page.locator('.thinking summary').first().click()
  const traces = await page.locator('.message.manager .thinking .trace').allTextContents()
  assert.ok(traces.some(trace => trace.includes('manager explicit trace')), traces.join(' | '))
  assert.equal(await page.getByText('worker body must stay out of chat').count(), 0)
  await page.reload()
  await page.waitForSelector('#connection-status:text-is("Connected")')
  await page.waitForSelector('.message-body:has-text("authoritative manager reply")')
  assert.equal(await page.locator('.message.manager').count(), 1)
  assert.deepEqual(errors, [])
  console.log('Real Go gateway + bundled React + SSE reconnect: PASS')
} finally { await browser.close() }
