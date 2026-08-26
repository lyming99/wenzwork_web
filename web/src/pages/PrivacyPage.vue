<script setup lang="ts">
import { usePageHead } from '@/composables/usePageHead'

usePageHead({
  title: '隐私政策',
  description: '了解 WenzWork 网站与账户服务计划收集的数据、使用目的、保留方式和你的权利。',
  path: '/privacy',
})
</script>

<template>
  <article class="policy-page">
    <header class="policy-header">
      <div class="shell policy-shell">
        <p class="section-kicker">隐私政策</p>
        <h1>对数据保持克制，<br />对用途保持透明。</h1>
        <p class="page-lead">
          本页是上线前审阅草案，不代表政策已经生效。责任主体、联系邮箱、处理商与保留期确认后会发布正式版本。
        </p>
        <div class="policy-meta">
          <span>草案版本 v0.4</span>
          <span>更新日期 2026-07-22</span>
          <span>尚未生效</span>
        </div>
      </div>
    </header>

    <div class="shell policy-layout">
      <nav class="policy-nav" aria-label="隐私政策目录">
        <strong>本页目录</strong>
        <a href="#scope">适用范围</a>
        <a href="#collection">收集的数据</a>
        <a href="#purpose">使用目的</a>
        <a href="#storage">存储与保留</a>
        <a href="#sharing">处理商与共享</a>
        <a href="#rights">你的权利</a>
        <a href="#security">安全措施</a>
        <a href="#children">未成年人</a>
        <a href="#changes">政策更新</a>
        <a href="#contact">联系我们</a>
      </nav>

      <div class="policy-content prose">
        <section id="scope">
          <h2>1. 适用范围</h2>
          <p>
            本政策草案适用于 WenzWork
            官方网站、账户中心、会员兑换和管理服务。桌面应用对本地文件的具体处理方式将在首个可下载版本确定后补充。第三方网站或你自行选择的同步服务不受本政策控制。
          </p>
        </section>

        <section id="collection">
          <h2>2. 我们计划收集的数据</h2>
          <ul>
            <li>
              <strong>账户数据：</strong
              >邮箱地址、显示名称、密码散列、邮箱验证状态和账户状态；不保存明文密码。
            </li>
            <li>
              <strong>安全与会话数据：</strong>会话标识、登录时间、登录 IP、由 IP
              解析的地区、粗粒度设备信息、失败次数和用于防滥用的网络信息。
            </li>
            <li>
              <strong>网站访问数据：</strong>访问时间、页面路径、IP
              地址、国家或地区、省市、来源网站主机名和浏览器摘要；页面查询参数与完整来源地址不会写入访问统计。
            </li>
            <li>
              <strong>会员数据：</strong>会员等级、有效期与兑换事件。兑换码仅保存 HMAC
              摘要和尾号，不保存可再次使用的完整明文。
            </li>
            <li>
              <strong>下载数据：</strong>下载的版本资产、下载时间、IP 地址和浏览器摘要，用于统计访问
              IP 是否在同一自然日下载；不会据此读取本地文件内容。
            </li>
            <li><strong>支持数据：</strong>你主动提交的问题、联系信息和必要附件。</li>
          </ul>
          <p>
            网站不会为了广告画像读取你的本地项目文件、AI
            对话、终端输入输出或任务正文。若未来提供云同步或新的遥测能力，将在启用前另行说明并提供适当选择。
          </p>
        </section>

        <section id="purpose">
          <h2>3. 使用目的与依据</h2>
          <p>
            数据用于创建和保护账户、维持登录会话、统计网站访问、地区分布以及同日下载和注册转化、排查异常登录、完成会员兑换、提供软件下载、响应支持请求、防止滥用并履行必要法律义务。我们不会出售个人数据，也不会把账户数据用于第三方定向广告。
          </p>
        </section>

        <section id="storage">
          <h2>4. 存储、跨境与保留</h2>
          <p>
            生产区域、基础设施供应商、是否发生跨境处理以及访问
            IP、访问、下载、注册统计和登录记录的具体保留期尚未最终选择。管理员查询访问统计或登录记录时，服务端会优先使用本地地区数据库；未命中的公网
            IP 会发送给 Toolshu 查询，服务不可用时再发送给 ip-api.com。成功的归属地结果在数据库缓存
            30
            天，失败结果仅短暂缓存，事件写入阶段不会调用这些接口。正式政策会逐项列明保留期限。账户删除后，业务数据将在验证请求并满足安全、争议处理与法定义务后删除或匿名化；备份中的残留会按轮换周期清除。
          </p>
        </section>

        <section id="sharing">
          <h2>5. 处理商与共享</h2>
          <p>
            服务可能需要托管、对象存储、内容分发、事务邮件和错误监控处理商。IP 归属地查询计划使用
            Toolshu，并在其不可用时使用 ip-api.com；这两项服务会接收待查询的公网 IP
            和请求所必需的网络信息。其余供应商尚未确认，正式上线前会补充处理商类别、所在地、用途和必要保障。
          </p>
          <p>
            除获得你的授权、完成你请求的服务、保护用户与系统安全或依法响应有权机关外，我们不会向无关第三方披露个人数据。
          </p>
        </section>

        <section id="rights">
          <h2>6. 你的权利</h2>
          <p>
            在适用法律范围内，你可以请求访问、更正、导出或删除个人数据，撤回基于同意的处理，并对部分处理提出限制或异议。为防止冒用，我们会先核验请求人与账户的关系。
          </p>
        </section>

        <section id="security">
          <h2>7. 安全措施</h2>
          <p>
            计划措施包括 Argon2id 密码散列、服务端会话、安全 Cookie、CSRF
            防护、管理员多因素认证、最小权限、敏感操作审计、传输加密和密钥分离。任何系统都无法承诺绝对安全；确认事件后，我们会按法律与风险采取通知和处置措施。
          </p>
        </section>

        <section id="children">
          <h2>8. 未成年人</h2>
          <p>
            服务不面向未达到所在地可独立同意年龄的儿童。若发现未经适当授权收集了未成年人数据，我们会核实并删除。
          </p>
        </section>

        <section id="changes">
          <h2>9. 政策更新</h2>
          <p>
            正式政策会保留版本号与生效日期。重大变化将在生效前通过网站或账户内通知说明；不会用更新政策的方式追溯扩大既有数据用途。
          </p>
        </section>

        <section id="contact">
          <h2>10. 联系我们</h2>
          <p>
            如需咨询产品、反馈问题或提交隐私权利请求，可通过微信 <strong>lyming555</strong> 或邮箱
            <a href="mailto:44185539@qq.com">44185539@qq.com</a>
            联系我们。隐私责任主体名称与正式请求处理时限仍需在上线前完成法律审阅和公示。
          </p>
        </section>

        <aside class="draft-callout">
          <strong>上线门槛</strong>
          <p>
            责任主体、处理商、部署区域、保留期和适用法律尚未确认。本草案需要产品负责人和合格法律顾问审阅，不能直接作为生产政策发布。
          </p>
        </aside>
      </div>
    </div>
  </article>
</template>
