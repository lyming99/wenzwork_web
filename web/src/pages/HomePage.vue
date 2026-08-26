<script setup lang="ts">
import { useHead } from '@unhead/vue'
import { onMounted, ref } from 'vue'

import { listPricingPlans, type PricingPlan } from '@/api/catalog'
import { absoluteUrl, usePageHead } from '@/composables/usePageHead'
import {
  billingPeriodLabels,
  freePlanServiceCopy,
  formatPublicPricingPlanPrice,
  publicPricingPlanDescription,
} from '@/utils/pricing'

interface HomePricePlan {
  code: string
  name: string
  price: string
  unit: string
}

const homePricePlans = ref<HomePricePlan[]>([
  { code: 'free', name: 'Free', price: '¥0', unit: freePlanServiceCopy },
  { code: 'pro', name: 'Pro', price: '¥59', unit: '首年 · 续费 ¥99/年' },
])
const proBenefitsSummary = 'Pro 每月含 10 GB 远程流量（内测期间不限流量），支持最多 10 台设备在线。'
const homePricingNote = ref(proBenefitsSummary)

const toHomePricePlan = (plan: PricingPlan): HomePricePlan => {
  const code = plan.code.toLowerCase()

  return {
    code: plan.code,
    name: plan.name,
    price: code === 'pro' && plan.priceMinor === null ? '¥59' : formatPublicPricingPlanPrice(plan),
    unit:
      code === 'pro'
        ? '首年 · 续费 ¥99/年'
        : code === 'free'
          ? freePlanServiceCopy
          : plan.billingPeriod === 'redemption'
            ? ''
            : billingPeriodLabels[plan.billingPeriod],
  }
}

onMounted(async () => {
  try {
    const remotePlans = await listPricingPlans()
    if (remotePlans.length === 0) {
      homePricingNote.value = '当前没有可展示的已发布方案，请前往价格页查看最新信息。'
      return
    }

    homePricePlans.value = remotePlans.map(toHomePricePlan)
    const featuredPlan = remotePlans.find((plan) => plan.code === 'pro') ?? remotePlans.at(-1)
    const publishedDescription = featuredPlan ? publicPricingPlanDescription(featuredPlan) : ''
    homePricingNote.value = publishedDescription
      ? `${publishedDescription} ${proBenefitsSummary}`
      : proBenefitsSummary
  } catch {
    homePricingNote.value = `最新价格暂时未同步；${proBenefitsSummary}`
  }
})

usePageHead({
  title: 'WenzWork｜本机与远程 AI 项目工作台',
  description:
    'WenzWork 把 AI 对话、项目文件、真实终端和任务执行放进同一个工作台，支持在本机工作，也可通过端到端加密连接远程设备。',
  path: '/',
})

useHead({
  script: [
    {
      type: 'application/ld+json',
      textContent: JSON.stringify({
        '@context': 'https://schema.org',
        '@type': 'SoftwareApplication',
        name: 'WenzWork',
        applicationCategory: 'ProductivityApplication',
        operatingSystem: 'Windows, macOS, Linux',
        url: absoluteUrl('/'),
        featureList: [
          '多设备与多项目工作区',
          'AI 对话、目标、计划与工具调用',
          '项目文件、真实终端与任务管理',
          '24 小时在线远程控制',
          '端到端加密通信',
          '开源的 DSH 同款 Agent 算法',
        ],
        offers: [
          {
            '@type': 'Offer',
            name: 'Free',
            description: freePlanServiceCopy,
            price: '0',
            priceCurrency: 'CNY',
          },
          {
            '@type': 'Offer',
            name: 'Pro 年费会员（首年）',
            price: '59',
            priceCurrency: 'CNY',
          },
          {
            '@type': 'Offer',
            name: 'Pro 永久会员',
            price: '399',
            priceCurrency: 'CNY',
          },
        ],
      }),
    },
  ],
})
</script>

<template>
  <section class="hero-section home-hero">
    <div class="shell hero-grid">
      <div class="hero-copy">
        <div class="eyebrow"><span class="status-dot"></span> 本机与远程，一套 AI 项目工作台</div>
        <h1>让 AI 进入真实项目，<br /><span>随时接着做。</span></h1>
        <p class="hero-lead">
          WenzWork 把 AI
          对话、项目文件、真实终端和任务执行放进同一个工作区。坐在电脑前直接处理本机项目，离开后也能安全连接自己的设备，围绕同一份上下文继续推进。
        </p>
        <div class="hero-actions">
          <RouterLink class="button" to="/download">免费下载桌面端</RouterLink>
          <a class="text-link" href="#work-anywhere"
            >看看如何跨设备工作 <span aria-hidden="true">→</span></a
          >
        </div>
        <p class="hero-note">本机优先 · 项目级权限 · 远程内容端到端加密</p>
      </div>

      <figure class="workspace-preview">
        <figcaption class="sr-only">
          WenzWork 项目工作台示意：同一界面中可切换文件、终端、任务和 AI 对话。
        </figcaption>
        <div class="workspace-window" aria-hidden="true">
          <div class="workspace-topbar">
            <div class="workspace-traffic"><i></i><i></i><i></i></div>
            <div class="workspace-device-pill"><i></i><span>本机 · wenzwork_web</span></div>
            <span class="workspace-connection">已安全连接</span>
          </div>
          <div class="workspace-body">
            <aside class="workspace-sidebar">
              <div class="workspace-project">
                <span class="workspace-folder"></span><strong>wenzwork_web</strong><span>⌄</span>
              </div>
              <div class="workspace-nav">
                <span><i>F</i>文件管理</span>
                <span><i>&gt;_</i>终端管理</span>
                <span><i>✓</i>任务管理</span>
                <span class="active"><i>✦</i>AI 对话 <b>4</b></span>
              </div>
              <div class="workspace-chat-list">
                <small>当前项目</small>
                <strong>重新梳理官网价值</strong>
                <span>刚刚</span>
                <strong>检查远程连接状态</strong>
                <span>18 分钟前</span>
              </div>
              <div class="workspace-account"><i>W</i><strong>WenzWork</strong></div>
            </aside>

            <div class="workspace-canvas">
              <header>
                <strong>重新梳理官网价值</strong>
                <span><i></i>远程设备在线</span>
              </header>
              <div class="workspace-goal">
                <span>目标</span>
                <strong>让产品价值与真实能力一致</strong>
                <small>3 / 4 已完成</small>
              </div>
              <div class="workspace-thread">
                <div class="workspace-user-message">请基于当前项目，整理一份准确的产品介绍。</div>
                <div class="workspace-agent-message">
                  <span class="workspace-agent-label">✦ WenzWork Agent</span>
                  <p>
                    已对齐桌面端、网页端与移动端的实际能力。首页将聚焦在一个更清晰的主线：
                    <strong>项目、AI 与远程执行，在同一个工作台里连续发生。</strong>
                  </p>
                  <div class="workspace-tool-row">
                    <span>✓ 读取项目</span><span>✓ 核对能力</span><span>● 更新页面</span>
                  </div>
                </div>
              </div>
              <div class="workspace-composer">
                <span>继续推进这个项目…</span>
                <div><small>项目写入</small><b>↑</b></div>
              </div>
            </div>
          </div>
        </div>
      </figure>
    </div>

    <div class="shell home-proof-strip" aria-label="WenzWork 核心特性">
      <div><strong>一个项目</strong><span>文件、终端、任务与对话共用上下文</span></div>
      <div><strong>两种模式</strong><span>本机直接工作，远程安全接续</span></div>
      <div><strong>多端协同</strong><span>桌面、浏览器与移动端保持同一思路</span></div>
    </div>
  </section>

  <section class="advantages-section" aria-labelledby="advantages-title">
    <div class="shell">
      <div class="section-heading home-section-heading advantages-heading">
        <p class="section-kicker">项目优势</p>
        <h2 id="advantages-title">从多项目协作到远程执行，<br />一套工作台持续在线。</h2>
        <p>
          WenzWork 面向真实开发与内容项目，把跨设备接续、远程控制、安全通信和可审计的 Agent
          执行整合在同一条工作流中。
        </p>
      </div>
      <div class="advantage-grid">
        <article>
          <span>01</span><strong>多设备</strong>
          <p>桌面、浏览器与手机端接续同一工作上下文。</p>
        </article>
        <article>
          <span>02</span><strong>多项目</strong>
          <p>每个项目独立管理文件、终端、任务与 AI 对话。</p>
        </article>
        <article>
          <span>03</span><strong>远程控制</strong>
          <p>从已授权控制端访问设备上的项目能力。</p>
        </article>
        <article>
          <span>04</span><strong>24 小时在线</strong>
          <p>Device Agent 常驻运行，项目随设备在线可达。</p>
        </article>
        <article>
          <span>05</span><strong>开源</strong>
          <p>核心项目公开，代码与演进过程都可检查。</p>
        </article>
        <article>
          <span>06</span><strong>加密通信</strong>
          <p>远程项目内容在控制端与设备间端到端加密。</p>
        </article>
        <article>
          <span>07</span><strong>DSH 同款 Agent 算法</strong>
          <p>复用目标、计划、工具调度与持续执行思路。</p>
        </article>
      </div>
      <a
        class="text-link advantages-source-link"
        href="https://github.com/lyming99/wenzwork"
        target="_blank"
        rel="noopener noreferrer"
        >查看 WenzWork 开源代码 <span aria-hidden="true">↗</span></a
      >
    </div>
  </section>

  <section class="capabilities-section">
    <div class="shell">
      <div class="section-heading home-section-heading">
        <p class="section-kicker">一个工作台，四种核心能力</p>
        <h2>不只和 AI 聊天，<br />让它在清晰边界内推进项目。</h2>
        <p>
          每个项目都是独立工作区。你决定它能看到什么、能够做什么，对话、工具和执行记录都围绕当前项目展开。
        </p>
      </div>

      <div class="capability-grid">
        <article class="capability-card capability-card-ai">
          <div class="capability-card-top">
            <span class="capability-symbol">✦</span><small>AI WORKSPACE</small>
          </div>
          <h3>从一句问题，到一个可继续的目标</h3>
          <p>
            支持会话、附件、计划、工具调用与执行轨迹。长任务可以暂停、恢复和继续，不必每次从头解释。
          </p>
          <div class="capability-chips">
            <span>目标与计划</span><span>附件与图片</span><span>工具审批</span
            ><span>停止与继续</span>
          </div>
        </article>

        <article class="capability-card">
          <div class="capability-card-top">
            <span class="capability-symbol capability-symbol-file">F</span><small>FILES</small>
          </div>
          <h3>项目文件就在手边</h3>
          <p>浏览、预览、保存、上传与下载，所有操作都约束在当前项目边界内。</p>
        </article>

        <article class="capability-card">
          <div class="capability-card-top">
            <span class="capability-symbol capability-symbol-terminal">&gt;_</span
            ><small>TERMINAL</small>
          </div>
          <h3>真实终端，不是模拟输出</h3>
          <p>在已授权项目中打开交互终端，管理会话、输入输出与窗口大小。</p>
        </article>

        <article class="capability-card">
          <div class="capability-card-top">
            <span class="capability-symbol capability-symbol-task">✓</span><small>TASKS</small>
          </div>
          <h3>让执行过程可见、可控</h3>
          <p>创建任务，跟踪状态和日志，并在需要时取消、重试或恢复工作流。</p>
        </article>
      </div>
    </div>
  </section>

  <section id="work-anywhere" class="continuity-section">
    <div class="shell continuity-layout">
      <div class="section-heading home-section-heading">
        <p class="section-kicker">本机与远程</p>
        <h2>电脑前深入处理，<br />换个设备也能继续。</h2>
        <p>
          本机模式直接使用你选择的文件夹；远程模式则在设备授权后，通过桌面端、浏览器或手机端回到同一个项目。
        </p>
      </div>

      <div class="mode-card-grid">
        <article class="mode-card mode-card-local">
          <div class="mode-card-heading"><span>LOCAL</span><i></i><small>本机模式</small></div>
          <h3>直接在自己的文件夹上工作</h3>
          <p>
            添加现有项目即可开始，不会移动或复制原文件。AI、文件、终端和任务都在本机工作区里协同。
          </p>
          <ul>
            <li>原有目录结构保持不变</li>
            <li>每个项目独立管理</li>
            <li>本机工作不依赖远程设备</li>
          </ul>
        </article>

        <article class="mode-card mode-card-remote">
          <div class="mode-card-heading"><span>REMOTE</span><i></i><small>远程模式</small></div>
          <h3>安全回到另一台设备上的项目</h3>
          <p>目标设备在线且已授权时，可通过加密通道继续 AI 对话，并访问项目文件、终端与任务。</p>
          <ul>
            <li>设备身份与短期会话凭证</li>
            <li>按项目和能力限定权限</li>
            <li>网络中断后分层恢复</li>
          </ul>
        </article>
      </div>
    </div>
  </section>

  <section class="privacy-promise-section">
    <div class="shell privacy-promise-card">
      <div class="privacy-promise-copy">
        <p class="section-kicker">安全边界</p>
        <h2>云端负责身份、授权与会合，<br />项目正文留在设备之间。</h2>
        <p>
          远程会话中的 AI
          提示与回复、附件、文件内容、终端输入输出和完整任务日志不由控制服务保存；它们在控制端与目标设备之间加密传输。
        </p>
      </div>
      <div class="privacy-promise-grid">
        <article>
          <span>01</span><strong>端到端加密</strong>
          <p>中继节点转发密文，不解密项目内容。</p>
        </article>
        <article>
          <span>02</span><strong>项目级范围</strong>
          <p>远程操作与已登记项目绑定，不默认放开整台设备。</p>
        </article>
        <article>
          <span>03</span><strong>工具权限可控</strong>
          <p>只读、工作区写入与完全访问分层选择；高权限模式必须由用户明确开启。</p>
        </article>
        <article>
          <span>04</span><strong>凭证短期有效</strong>
          <p>会话与设备身份绑定，过期、撤销或设备迁移后需重新建立授权。</p>
        </article>
      </div>
    </div>
  </section>

  <section class="home-download-section">
    <div class="shell home-download-card">
      <div>
        <p class="section-kicker">一套工作模式，适应不同设备</p>
        <h2>桌面端深入工作，浏览器与手机端远程接续。</h2>
        <p>当前可下载的系统、架构、签名状态与 SHA-256 以下载页实时目录为准。</p>
      </div>
      <div class="platform-pills" aria-label="WenzWork 使用方式">
        <span>桌面端 <small>完整工作台</small></span>
        <span>浏览器 <small>远程控制</small></span>
        <span>移动端 <small>随身接续</small></span>
      </div>
      <RouterLink class="button" to="/download">查看官方版本</RouterLink>
    </div>
  </section>

  <section class="home-pricing-section">
    <div class="shell home-pricing-grid">
      <div>
        <p class="section-kicker">Free 与 Pro</p>
        <h2>先免费开始，再按需要选择 Pro。</h2>
        <p class="home-pricing-intro">
          Free 自部署服务免费；Pro 首年 59 元并赠送 WenzMark 会员，次年起 99 元/年；永久会员 399
          元，限量 50 位。
        </p>
      </div>
      <div class="home-price-summary">
        <div v-for="plan in homePricePlans" :key="plan.code">
          <strong>{{ plan.name }}</strong>
          <span
            >{{ plan.price }}<template v-if="plan.unit"> · {{ plan.unit }}</template></span
          >
        </div>
        <div>
          <strong>永久 Pro</strong>
          <span>¥399 · 一次购买 · 限量 50 位</span>
        </div>
        <p aria-live="polite">{{ homePricingNote }}</p>
        <RouterLink class="text-link" to="/pricing"
          >比较会员方案 <span aria-hidden="true">→</span></RouterLink
        >
      </div>
    </div>
  </section>

  <section class="faq-section home-faq">
    <div class="shell faq-grid">
      <div>
        <p class="section-kicker">常见问题</p>
        <h2>开始前，先了解这些关键边界。</h2>
      </div>
      <div class="faq-list">
        <details>
          <summary>WenzWork 和普通 AI 聊天工具有什么不同？</summary>
          <p>
            WenzWork 以真实项目为边界，把对话与文件、终端、任务和工具权限连在一起。AI
            只在你选择的范围内读取或执行，并保留可见的过程。
          </p>
        </details>
        <details>
          <summary>远程使用时，项目内容会被上传到云端吗？</summary>
          <p>
            控制服务会处理账户、设备、授权和连接路由所需的元数据；AI
            对话正文、文件内容、终端输入输出与完整任务日志在控制端和目标设备之间端到端加密，不由控制服务保存。
          </p>
        </details>
        <details>
          <summary>本机模式必须一直联网吗？</summary>
          <p>
            本机工作区直接使用当前设备上的项目；需要第三方 AI
            模型时，是否联网取决于你配置的模型服务。远程模式则需要网络，且目标设备必须在线并已开启远程访问。
          </p>
        </details>
        <details>
          <summary>现在可以下载 WenzWork 吗？</summary>
          <p>
            可以。产品仍在内测，可用平台、版本与校验信息以下载页为准；如果遇到问题，欢迎加入 QQ
            交流群 1026582431 反馈。
          </p>
        </details>
      </div>
    </div>
  </section>

  <section class="membership-callout">
    <div class="shell callout-card">
      <div>
        <p class="section-kicker">WenzWork Beta</p>
        <h2>从一个真实项目开始。</h2>
        <p>下载桌面端，添加自己的项目文件夹，再决定 AI 能读取什么、能执行什么。</p>
      </div>
      <div class="callout-actions">
        <RouterLink class="button button-light" to="/download">免费下载</RouterLink>
        <RouterLink class="callout-link" to="/help">查看使用帮助 →</RouterLink>
      </div>
    </div>
  </section>
</template>
