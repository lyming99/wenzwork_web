import { useHead } from '@unhead/vue'

const fallbackSiteUrl = 'http://localhost:5173'

const resolveSiteUrl = () => {
  try {
    return new URL(import.meta.env.VITE_SITE_URL || fallbackSiteUrl).origin
  } catch {
    return fallbackSiteUrl
  }
}

export const siteUrl = resolveSiteUrl()

export const absoluteUrl = (path: string) => new URL(path, `${siteUrl}/`).toString()
export const socialPreviewUrl = absoluteUrl('/og.png')

interface PageHeadOptions {
  title: string
  description: string
  path: string
}

export const usePageHead = ({ title, description, path }: PageHeadOptions) => {
  const fullTitle = title.includes('WenzWork') ? title : `${title}｜WenzWork`
  const canonical = absoluteUrl(path)

  useHead({
    title: fullTitle,
    link: [{ rel: 'canonical', href: canonical }],
    meta: [
      { name: 'description', content: description },
      { property: 'og:type', content: 'website' },
      { property: 'og:locale', content: 'zh_CN' },
      { property: 'og:site_name', content: 'WenzWork' },
      { property: 'og:title', content: fullTitle },
      { property: 'og:description', content: description },
      { property: 'og:url', content: canonical },
      { property: 'og:image', content: socialPreviewUrl },
      { property: 'og:image:width', content: '1736' },
      { property: 'og:image:height', content: '906' },
      {
        property: 'og:image:alt',
        content: 'WenzWork 本机与远程 AI 项目工作台',
      },
      { name: 'twitter:card', content: 'summary_large_image' },
      { name: 'twitter:title', content: fullTitle },
      { name: 'twitter:description', content: description },
      { name: 'twitter:image', content: socialPreviewUrl },
      {
        name: 'twitter:image:alt',
        content: 'WenzWork 本机与远程 AI 项目工作台',
      },
    ],
  })
}
