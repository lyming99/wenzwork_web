import { mkdir, writeFile } from 'node:fs/promises'
import { resolve } from 'node:path'

const outputDirectory = resolve('dist/server')
const outputFile = resolve(outputDirectory, 'index.js')

const workerSource = `const htmlRequest = (request, pathname) => {
  const url = new URL(request.url)
  url.pathname = pathname
  return new Request(url, request)
}

const withWebHeaders = (response, pathname) => {
  const headers = new Headers(response.headers)
  const contentType = headers.get('Content-Type') || ''
  if (contentType.includes('text/html') || pathname.endsWith('.html')) {
    headers.set('Cache-Control', 'no-cache')
  } else if (pathname.startsWith('/assets/')) {
    headers.set('Cache-Control', 'public, max-age=31536000, immutable')
  } else {
    headers.set('Cache-Control', 'public, max-age=300')
  }
  headers.set('X-Content-Type-Options', 'nosniff')
  headers.set('Referrer-Policy', 'strict-origin-when-cross-origin')
  headers.set('Permissions-Policy', 'camera=(), microphone=(), geolocation=()')
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  })
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url)
    const directResponse = await env.ASSETS.fetch(request)
    if (directResponse.status !== 404 || !['GET', 'HEAD'].includes(request.method)) {
      return ['GET', 'HEAD'].includes(request.method)
        ? withWebHeaders(directResponse, url.pathname)
        : directResponse
    }

    const pathname = url.pathname.replace(/\\/+$/, '') || '/'
    const candidates = pathname === '/'
      ? ['/index.html']
      : [\`\${pathname}.html\`, \`\${pathname}/index.html\`]

    for (const candidate of candidates) {
      const response = await env.ASSETS.fetch(htmlRequest(request, candidate))
      if (response.status !== 404) return withWebHeaders(response, candidate)
    }

    return withWebHeaders(await env.ASSETS.fetch(htmlRequest(request, '/404.html')), '/404.html')
  },
}
`

await mkdir(outputDirectory, { recursive: true })
await writeFile(outputFile, workerSource, 'utf8')
