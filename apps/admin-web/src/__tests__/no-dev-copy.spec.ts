import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { describe, expect, it } from 'vitest'

/**
 * 仓库级守卫：**用户看得见的文案里不许出现"给开发者看"的东西**。
 *
 * WHY 需要一条自动化的守卫，而不是靠 review：这些文字本来都写在注释里，是人手把它们
 * 复制到模板里时"顺手带上"的（§4、Phase 1、make create-admin、"后端也会拒绝"…）。
 * 它们在代码评审里毫不起眼，却会直接显示给学生/老师/管理员，让人以为系统缺功能或没做完。
 * 放到测试里，才能在下一次"顺手带上"时立刻失败。
 *
 * 放在 admin-web 下的原因：这里是第一次发现泄漏的地方，而 CI 已经会跑各 app 的测试，
 * 不必为了这条守卫新增一套测试基础设施。它扫描的是整个 apps/ 与 shared-types。
 *
 * 判定范围（只查**会被渲染**的部分）：
 *  - .vue 的 <template> 去掉 HTML 注释之后的文本节点与可见属性；
 *  - 面向用户的消息表（messages / status / private-talk / api-error 模块）里的字符串面量。
 * 代码注释不查——那是 WHY 文档，本来就应该有 §/Phase 的出处。
 */
// vitest 的 cwd 就是当前包目录（apps/admin-web），仓库根在两层之上。
// 不用 import.meta.url：happy-dom 环境下它是 http:// 而不是 file:，fileURLToPath 会拒绝。
const REPO = join(process.cwd(), '..', '..')
if (!existsSync(join(REPO, 'apps'))) {
  throw new Error(
    `守卫测试找不到仓库根（推出来是 ${REPO}）：请确认 vitest 的 cwd 是 apps/admin-web`,
  )
}

/** 只有这些属性会被渲染给用户；class/style/data-* 等不算。 */
const VISIBLE_ATTRS = 'hint|description|title|label|placeholder|aria-label|note|error|message'

/** 开发期用语：规范条款、阶段编号、服务器命令、实现细节。 */
const DEV_TALK = /§|Phase\s*\d|\bV1\b|make\s+create-admin|adminctl|缺功能|不是缺功能|自助注册入口/

function walk(dir: string, ext: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    if (name === 'node_modules' || name === 'dist' || name === '__tests__') continue
    const full = join(dir, name)
    if (statSync(full).isDirectory()) walk(full, ext, out)
    else if (name.endsWith(ext)) out.push(full)
  }
  return out
}

/** 摘出模板中真正会渲染的文字。 */
function visibleTemplateText(source: string): string[] {
  const match = /<template>([\s\S]*)<\/template>/.exec(source)
  const tpl = match?.[1]
  if (tpl === undefined) return []
  const noComments = tpl.replace(/<!--[\s\S]*?-->/g, ' ')
  const flat = noComments.replace(/\n/g, ' ')
  const found: string[] = []
  for (const m of flat.matchAll(/>([^<>{}]+)</g)) {
    const raw = m[1]
    if (raw === undefined) continue
    const text = raw.replace(/\s+/g, ' ').trim()
    if (text.length > 3) found.push(text)
  }
  for (const m of flat.matchAll(new RegExp(`\\b(${VISIBLE_ATTRS})="([^"]*)"`, 'g'))) {
    const value = m[2]
    if (value !== undefined) found.push(value)
  }
  return found
}

/** 摘出消息表里"字符串字面量"形式的用户文案。 */
function userFacingStrings(source: string): string[] {
  return source
    .split('\n')
    .filter((line) => {
      const t = line.trim()
      return !t.startsWith('*') && !t.startsWith('//') && !t.startsWith('/*')
    })
    .flatMap((line) =>
      [...line.matchAll(/'([^']{8,})'|"([^"]{8,})"/g)]
        .map((m) => m[1] ?? m[2])
        .filter((v): v is string => v !== undefined),
    )
}

describe('面向用户的文案里不含开发期内容', () => {
  it('所有前端模板的可见文字都不含 §/Phase/服务器命令/实现细节', () => {
    const offenders: string[] = []
    for (const file of walk(join(REPO, 'apps'), '.vue')) {
      for (const text of visibleTemplateText(readFileSync(file, 'utf8'))) {
        if (DEV_TALK.test(text)) offenders.push(`${relative(REPO, file)} → ${text.slice(0, 110)}`)
      }
    }
    expect(offenders, `界面里出现了给开发者看的文字：\n${offenders.join('\n')}`).toEqual([])
  })

  it('面向用户的消息表（错误/状态/提示文案）同样干净', () => {
    const targets = [
      ...walk(join(REPO, 'apps'), '.ts').filter((f) =>
        /(messages|status|private-talk|auth-api)\.ts$/.test(f),
      ),
      join(REPO, 'packages/shared-types/src/api-error.ts'),
    ]
    const offenders: string[] = []
    for (const file of targets) {
      for (const text of userFacingStrings(readFileSync(file, 'utf8'))) {
        if (DEV_TALK.test(text)) offenders.push(`${relative(REPO, file)} → ${text.slice(0, 110)}`)
      }
    }
    expect(offenders, `文案里出现了给开发者看的内容：\n${offenders.join('\n')}`).toEqual([])
  })
})
