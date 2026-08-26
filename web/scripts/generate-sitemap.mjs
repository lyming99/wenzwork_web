import { readdir, writeFile } from 'node:fs/promises'
import { resolve } from 'node:path'

const projectRoot = resolve(import.meta.dirname, '..')
const outputDirectory = resolve(projectRoot, 'dist')
const helpDirectory = resolve(projectRoot, 'src', 'content', 'help')

const resolveSiteOrigin = () => {
  const configured = process.env.VITE_SITE_URL || 'http://localhost:5173'

  try {
    return new URL(configured).origin
  } catch {
    throw new Error(`VITE_SITE_URL 必须是绝对 URL，当前值为 ${configured}`)
  }
}

const siteOrigin = resolveSiteOrigin()
const helpRoutes = (await readdir(helpDirectory))
  .filter((filename) => filename.endsWith('.md'))
  .map((filename) => `/help/${filename.slice(0, -3)}`)
  .sort()

const routes = ['/', '/help', ...helpRoutes, '/download', '/pricing', '/privacy']
const escapeXml = (value) =>
  value.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;')
const absolute = (path) => new URL(path, `${siteOrigin}/`).toString()

const sitemap = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
${routes.map((route) => `  <url><loc>${escapeXml(absolute(route))}</loc></url>`).join('\n')}
</urlset>
`

const robots = `User-agent: *
Allow: /
Disallow: /account
Disallow: /admin
Disallow: /login

Sitemap: ${absolute('/sitemap.xml')}
`

await Promise.all([
  writeFile(resolve(outputDirectory, 'sitemap.xml'), sitemap, 'utf8'),
  writeFile(resolve(outputDirectory, 'robots.txt'), robots, 'utf8'),
])

process.stdout.write(`Generated sitemap.xml and robots.txt for ${routes.length} public routes.\n`)
