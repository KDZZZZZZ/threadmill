import { mkdir, writeFile } from 'node:fs/promises'

const pages = ['components/button', 'components/modal', 'components/toast', 'getting-started/quick-start']
await mkdir('.heroui-docs/react', { recursive: true })
for (const page of pages) {
  const response = await fetch(`https://heroui.com/docs/react/${page}.mdx`)
  if (!response.ok) throw new Error(`${page}: HTTP ${response.status}`)
  await writeFile(`.heroui-docs/react/${page.replace('/', '-')}.mdx`, await response.text())
}
await writeFile('.heroui-AGENTS.md', '# HeroUI v3 local documentation\n\n' + pages.map(page =>
  `- [${page}](.heroui-docs/react/${page.replace('/', '-')}.mdx)\n`).join(''))
