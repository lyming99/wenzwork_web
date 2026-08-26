import { describe, expect, it } from 'vitest'

import { helpArticles, parseHelpArticle, renderSafeMarkdown } from './help'

describe('help content pipeline', () => {
  it('loads and orders repository Markdown articles', () => {
    expect(helpArticles.length).toBeGreaterThanOrEqual(9)
    expect(helpArticles[0]?.slug).toBe('getting-started')
    expect(helpArticles.every((article) => article.html.includes('<h1>'))).toBe(true)
    expect(helpArticles.slice(0, 3).map((article) => article.slug)).toEqual([
      'getting-started',
      'deploy-host-one-click',
      'connect-remote-device',
    ])
    expect(
      helpArticles.find((article) => article.slug === 'deploy-host-one-click')?.html,
    ).toContain('下载 Bash 一键安装脚本')
    expect(
      helpArticles.find((article) => article.slug === 'connect-remote-device')?.html,
    ).toContain('生成 Access Key')
    expect(helpArticles.find((article) => article.slug === 'deploy-relay')?.html).toContain(
      'Relay Access Key',
    )
  })

  it('drops raw HTML and unsafe link protocols', () => {
    const html = renderSafeMarkdown(
      '# Safe\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1))\n\n**kept**',
    )

    expect(html).not.toContain('<script')
    expect(html).not.toContain('javascript:')
    expect(html).toContain('<strong>kept</strong>')
  })

  it('rejects malformed article metadata and unsafe slugs', () => {
    expect(() => parseHelpArticle('../Bad Name.md', 'missing')).toThrow()
    expect(() =>
      parseHelpArticle(
        './valid.md',
        '---\ntitle: Title\ndescription: Description\ncategory: Test\norder: no\nupdatedAt: 2026-07-21\n---\nBody',
      ),
    ).toThrow('order 必须是整数')
  })
})
