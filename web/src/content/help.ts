import rehypeSanitize from 'rehype-sanitize'
import rehypeStringify from 'rehype-stringify'
import remarkParse from 'remark-parse'
import remarkRehype from 'remark-rehype'
import { unified } from 'unified'

export interface HelpArticle {
  slug: string
  title: string
  description: string
  category: string
  order: number
  updatedAt: string
  html: string
  searchText: string
}

type Frontmatter = Omit<HelpArticle, 'slug' | 'html' | 'searchText'>

const requiredFields = ['title', 'description', 'category', 'order', 'updatedAt'] as const

const parseFrontmatter = (
  source: string,
  filename: string,
): { data: Frontmatter; body: string } => {
  const match = source.match(/^---\r?\n([\s\S]*?)\r?\n---\r?\n?([\s\S]*)$/)

  if (!match) {
    throw new Error(`帮助文章 ${filename} 缺少有效的 frontmatter`)
  }

  const entries = Object.fromEntries(
    match[1]
      .split(/\r?\n/)
      .filter(Boolean)
      .map((line) => {
        const separator = line.indexOf(':')
        if (separator < 1) throw new Error(`帮助文章 ${filename} 的 frontmatter 格式错误`)
        return [line.slice(0, separator).trim(), line.slice(separator + 1).trim()]
      }),
  )

  for (const field of requiredFields) {
    if (!entries[field]) throw new Error(`帮助文章 ${filename} 缺少 ${field}`)
  }

  const order = Number(entries.order)
  if (!Number.isSafeInteger(order)) throw new Error(`帮助文章 ${filename} 的 order 必须是整数`)

  return {
    data: {
      title: entries.title,
      description: entries.description,
      category: entries.category,
      order,
      updatedAt: entries.updatedAt,
    },
    body: match[2],
  }
}

export const renderSafeMarkdown = (source: string) =>
  unified()
    .use(remarkParse)
    .use(remarkRehype)
    .use(rehypeSanitize)
    .use(rehypeStringify)
    .processSync(source)
    .toString()

export const parseHelpArticle = (filename: string, source: string): HelpArticle => {
  const slug = filename.replace(/^.*\//, '').replace(/\.md$/, '')
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(slug)) {
    throw new Error(`帮助文章文件名 ${filename} 不是安全的 slug`)
  }

  const { data, body } = parseFrontmatter(source, filename)
  const searchText = `${data.title} ${data.description} ${data.category} ${body}`
    .replace(/[`*_#[\]()>-]/g, ' ')
    .replace(/\s+/g, ' ')
    .toLocaleLowerCase('zh-CN')

  return {
    slug,
    ...data,
    html: renderSafeMarkdown(body),
    searchText,
  }
}

const markdownModules = import.meta.glob('./help/*.md', {
  eager: true,
  import: 'default',
  query: '?raw',
}) as Record<string, string>

export const helpArticles = Object.entries(markdownModules)
  .map(([filename, source]) => parseHelpArticle(filename, source))
  .sort((left, right) => left.order - right.order)

export const helpArticlePaths = helpArticles.map((article) => `/help/${article.slug}`)

export const getHelpArticle = (slug: string) =>
  helpArticles.find((article) => article.slug === slug)
