import assert from 'node:assert/strict'
import { mkdir, readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright-core'

const origin = process.env.WEBUI_URL ?? 'http://127.0.0.1:5174'
const directory = fileURLToPath(new URL('.', import.meta.url))
const browser = await chromium.launch({ headless: true, ...(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : {}) })
const errors = []
async function page(options = {}) {
  const context = await browser.newContext({ viewport: { width: 1680, height: 1020 }, ...options })
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  return page
}
async function until(page, predicate, arg) { await page.waitForFunction(predicate, arg, { timeout: 15_000 }) }

try {
  // The original, green characterization checks now run against the built React UI.
  for (const kind of ['message-order', 'help']) {
    const p = await page()
    await p.addInitScript(await readFile(`${directory}fixtures/${kind}.js`, 'utf8'))
    await p.goto(origin)
    await until(p, kind => document.querySelector(`#${kind === 'help' ? 'help-check' : 'message-order'}-results`)?.dataset.failures !== undefined, kind)
    const result = await p.locator(`#${kind === 'help' ? 'help-check' : 'message-order'}-results`).evaluate(node => ({ failures: node.dataset.failures, text: node.textContent }))
    assert.equal(result.failures, '0', result.text)
    console.log(result.text.trim())
    await p.context().close()
  }

  const p = await page()
  await p.addInitScript(() => {
    const project = { id: 'live-test', name: 'Live test', root: '/fixture/live', runtime_state: 'open', busy: false, pending: 0 }
    const other = { ...project, id: 'other', name: 'Other', root: '/fixture/other' }
    let messages = [], sequence = 0
    const streams = new Set(), requests = []
    let failSend = true
    const snapshot = () => ({ project, messages: { items: messages }, agents: [], graph: { tasks: [], edges: [] } })
    const emit = (name, data, seq = ++sequence, id = project.id) => {
      for (const stream of streams) if (stream.path.includes(`/${id}/`)) stream.dispatchEvent(new MessageEvent(name, { data: JSON.stringify({ project_id: id, seq: String(seq), data }) }))
    }
    window.__fixture = {
      requests, emit,
      snapshot: () => snapshot(),
      messages(items) { messages = items; emit('snapshot', snapshot()) },
      state(state) { Object.assign(project, state); emit('snapshot', snapshot()) },
      closeViaHTTP() { Object.assign(project, { runtime_state: 'closed', busy: false }) },
      disconnect() { for (const stream of streams) stream.onerror?.() },
      reconnect() { for (const stream of streams) stream.onopen?.(); emit('snapshot', snapshot()) },
    }
    window.fetch = async (path, options) => {
      if (project.runtime_state === 'closed' && path.endsWith('/graph')) return new Response(JSON.stringify({ error: { message: 'project is closed' } }), { status: 409 })
      if (options?.method === 'POST') {
        const body = JSON.parse(options.body); requests.push({ path, body })
        if (path.endsWith('/messages')) {
          if (failSend) { failSend = false; return new Response(JSON.stringify({ error: { message: 'Temporary send error' } }), { status: 503 }) }
          messages.push({ id: 'sent', speaker: 'user', content: body.content, status: 'queued' })
          return new Response(JSON.stringify({ accepted: true, message_id: 'sent', queue_depth: 1 }))
        }
        if (!body.root.startsWith('/')) return new Response(JSON.stringify({ error: { message: 'Enter an absolute path.' } }), { status: 400 })
        return new Response(JSON.stringify(body.root === other.root ? other : project))
      }
      const result = path === '/api/v1/projects' ? { items: [project, other] }
        : path.endsWith('/messages') ? { items: path.includes('/other/') ? [] : messages }
          : path.endsWith('/agents') ? { items: [] }
            : path.endsWith('/graph') ? { tasks: [], edges: [] } : path.endsWith('/other') ? other : project
      return new Response(JSON.stringify(result))
    }
    window.EventSource = class extends EventTarget {
      constructor(path) {
        super(); this.path = path; streams.add(this)
        setTimeout(() => { if (!streams.has(this)) return; this.onopen?.(); emit('snapshot', path.includes('/other/') ? { ...snapshot(), project: other, messages: { items: [] } } : snapshot(), ++sequence, path.includes('/other/') ? other.id : project.id) }, 0)
      }
      close() { streams.delete(this) }
    }
  })
  await p.goto(origin)
  await p.waitForSelector('#connection-status:text-is("Connected")')
  await p.locator('#prompt').fill('Keep this draft')
  await p.locator('[data-project="other"]').click()
  await p.waitForSelector('[data-project="other"].selected')
  assert.equal(await p.locator('#prompt').inputValue(), '')
  await p.locator('[data-project="live-test"]').click()
  await p.waitForSelector('[data-project="live-test"].selected')
  assert.equal(await p.locator('#prompt').inputValue(), 'Keep this draft')
  await p.locator('#send').click()
  await p.waitForSelector('#toast:text-is("Temporary send error")')
  assert.equal(await p.locator('#prompt').inputValue(), 'Keep this draft')
  await p.locator('#send').click()
  await until(p, () => document.querySelector('#prompt').value === '')
  const sent = await p.evaluate(() => window.__fixture.requests.filter(request => request.path.endsWith('/messages')))
  assert.equal(sent.length, 2)
  assert.equal(sent[0].body.client_message_id, sent[1].body.client_message_id, 'Retry must reuse the receipt key')
  assert.equal(await p.locator('.message.user').count(), 1)
  console.log('PASS Project drafts, retry identity and message delivery')

  await p.evaluate(() => window.__fixture.disconnect())
  await until(p, () => document.querySelector('#connection-status').textContent === 'Connecting…')
  await p.evaluate(() => window.__fixture.reconnect())
  await p.waitForSelector('#connection-status:text-is("Connected")')
  await p.evaluate(() => {
    const fixture = window.__fixture
    fixture.emit('runtime_event', { agent_id: 'manager', kind: 'model', phase: 'delta', delta: 'stale' }, 0)
    fixture.emit('output', { id: 'wrong', speaker: 'manager', content: 'wrong project' }, 999, 'other')
  })
  assert.equal(await p.getByText('wrong project', { exact: true }).count(), 0)
  console.log('PASS Reconnect and event isolation')

  const markdown = '## 标题\n\n**粗体** *斜体* ~~删除~~\n\n- 父项\n  - 子项\n\n1. 步骤\n\n> 引用\n\n```html\n  <b>& literal</b>\n```\n\n| A | B |\n| --- | ---: |\n| 中文 | 1 |\n\n- [x] Done\n- [ ] Pending\n\n[Docs](https://example.com/docs?q=one&b=two)\n\n[bad](javascript:alert(1))\n\n[bad](jav&#x61;script:alert(1))\n\n[bad](data:text/html,unsafe)\n\n<img src=x onerror="window.__markdownInjected=1"><script>window.__markdownInjected=1</script>\n\n![bad](javascript:alert(1))'
  await p.evaluate(content => window.__fixture.messages([{ id: 'markdown', speaker: 'manager', content }]), markdown)
  await p.waitForSelector('.markdown h2')
  assert.equal(await p.locator('.markdown h2').textContent(), '标题')
  for (const selector of ['strong', 'em', 'del', 'ul ul li', 'ol li', 'blockquote', '.md-table[tabindex="0"] table', 'pre[tabindex="0"] code']) assert.ok(await p.locator(`.markdown ${selector}`).count(), selector)
  assert.equal(await p.locator('.markdown pre code').textContent(), '  <b>& literal</b>\n')
  assert.equal(await p.locator('.markdown input[disabled]').count(), 2)
  assert.equal(await p.locator('.markdown input[checked]').count(), 1)
  assert.equal(await p.locator('.markdown a[href]').count(), 1)
  assert.equal(await p.locator('.markdown a[href]').getAttribute('rel'), 'noopener noreferrer')
  assert.equal(await p.locator('.markdown a[href]').getAttribute('target'), '_blank')
  assert.equal(await p.locator('.markdown script,.markdown iframe,.markdown img[src]').count(), 0)
  assert.ok((await p.locator('.markdown').textContent()).includes('<img'))
  assert.equal(await p.evaluate(() => window.__markdownInjected), undefined)
  await p.evaluate(() => window.__fixture.messages([{ id: 'markdown', speaker: 'manager', content: '```python\nprint("hello")' }]))
  await until(p, () => document.querySelector('.markdown pre code')?.textContent.includes('print("hello")'))
  await p.evaluate(() => window.__fixture.messages([{ id: 'markdown', speaker: 'manager', content: '```python\nprint("hello")\n```\n\n**Finished**' }]))
  await p.waitForSelector('.markdown strong:text-is("Finished")')
  console.log('PASS Markdown formatting, streaming and injection safety')
  await p.evaluate(() => window.__fixture.state({ runtime_state: 'error', busy: false, error: 'Runtime ended' }))
  await until(p, () => document.querySelector('#prompt').disabled)
  await p.evaluate(() => window.__fixture.closeViaHTTP())
  await p.waitForSelector('#connection-status:text-is("Closed")')
  console.log('PASS Closed project remains readable when graph API returns 409')
  await p.context().close()

  for (const mode of ['dark', 'light']) {
    const p = await page({ reducedMotion: 'reduce' })
    await p.addInitScript(mode => localStorage.setItem('threadmill-theme', mode), mode)
    await p.goto(`${origin}/?demo=1`)
    await p.waitForSelector('.graph-task')
    const box = await p.locator('#sidebar').boundingBox()
    assert.equal(Math.round(box.width), 224)
    assert.equal(Math.round((await p.locator('#inspector').boundingBox()).width), 390)
    assert.equal(await p.locator('.loading-state').count(), 1)
    assert.equal(await p.locator('.loader-grid i').first().evaluate(node => getComputedStyle(node).animationName), 'none')
    await p.locator('#graph').focus(); await p.keyboard.press('ArrowRight')
    assert.ok((await p.locator('#graph-content').getAttribute('style')).includes('32px'))
    await p.keyboard.press('+')
    assert.ok(!(await p.locator('#graph-content').getAttribute('style')).includes('scale(1)'))
    await p.keyboard.press('0')
    await p.locator('#sidebar-resize').focus(); await p.keyboard.press('Home')
    assert.equal(await p.locator('#sidebar-resize').getAttribute('aria-valuetext'), 'Collapsed')
    await p.keyboard.press('Enter')
    assert.equal(await p.locator('#sidebar-resize').getAttribute('aria-valuetext'), '224 pixels')
    await p.locator('#search-toggle').click(); await p.locator('#project-search').fill('docs')
    assert.equal(await p.locator('#project-list button').count(), 1)
    await p.locator('#search-close').click()
    await p.locator('#open-project').click(); await p.waitForSelector('#project-dialog[open]')
    await p.locator('#new-root').fill('/fixture/new-project')
    await p.locator('#project-submit').click(); await p.waitForSelector('#project-dialog', { state: 'hidden' })
    await p.waitForSelector('.nav-row.selected:has-text("new-project")')
    await p.getByRole('button', { name: 'threadmill', exact: true }).click()
    await p.waitForSelector('.graph-task')
    const shots = `${directory}${mode === 'dark' ? 'shots-dark' : 'shots'}`
    await mkdir(shots, { recursive: true })
    await p.screenshot({ path: `${shots}/workspace.png` })
    await p.setViewportSize({ width: 390, height: 844 })
    await p.locator('#inspect-toggle').click(); assert.ok(await p.locator('#inspector').isVisible())
    await p.locator('#inspect-close').click(); assert.equal(await p.locator('#inspector').isVisible(), false)
    await p.locator('#nav-toggle').click(); assert.ok(await p.locator('#sidebar').isVisible())
    await p.locator('#sidebar-toggle').click()
    await p.screenshot({ path: `${shots}/mobile.png` })
    console.log(`PASS ${mode} theme, reduced motion, panels, graph, projects and mobile`)
    await p.context().close()
  }
  assert.deepEqual(errors, [], 'Browser errors')
  console.log('Frontend E2E: PASS')
} finally { await browser.close() }
