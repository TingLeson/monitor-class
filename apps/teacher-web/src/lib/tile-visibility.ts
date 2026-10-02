/**
 * 网格卡片的可见性观察（§52 / §73）。
 *
 * §52 要求老师端"根据当前可见 Tile、Focus 学生、页面滚动位置动态订阅"。
 * 这一层只回答**一个**问题：某张卡片现在是不是在视口里。把答案交给 store，
 * 由 store 决定订不订、订多清楚——媒体策略只有一处真相，DOM 层不做业务判断。
 *
 * WHY 用 IntersectionObserver 而不是监听 scroll：
 * - scroll 事件在滚动过程中会以帧率触发，而我们只关心"越过视口边界"这一个瞬间；
 * - IO 由浏览器在合成阶段统一计算，卡片再多也只是一次回调，
 *   不需要自己去读 getBoundingClientRect（那会强制同步布局）。
 *
 * WHY 有一个可注入的工厂：happy-dom 没有布局引擎，真实 IO 在测试里永远不会回调。
 * 注入点让测试能精确地控制"哪张卡片在视口里"，从而把 §52 的订阅策略钉死；
 * 而真实实现仍然只有下面这一段，没有任何"测试专用分支"混进生产路径。
 */

/** 一张卡片的可见性观察句柄。 */
export interface TileVisibilityHandle {
  /** 开始观察这张卡片的根元素。 */
  observe(element: Element): void
  /** 停止观察（组件卸载）。 */
  disconnect(): void
}

/**
 * 可见性观察工厂。
 *
 * 签名里带 `studentId` 而真实实现并不使用它：**可见性是元素的属性，不是学生的属性**。
 * 这样做是为了让测试替身能把"哪个学生"与对应的那张卡片直接对上，
 * 而不必反查 DOM 结构——否则每写一个可见性用例都要先找到第 N 张卡片的元素。
 */
export type TileVisibilityFactory = (
  studentId: string,
  onChange: (visible: boolean) => void,
) => TileVisibilityHandle

/**
 * 默认实现：一张卡片一个 observer。
 *
 * `threshold: 0`：只要有一个像素进入视口就算可见。露出一角的卡片也该开始拉流，
 * 否则老师一滚动就会先看到一排"正在订阅画面…"，那正是监督墙最不该有的空窗。
 *
 * 刻意**不**设 `rootMargin`：留出预取边距确实能让滚动更顺，但它会让
 * "离开视口 → 取消订阅"这条约束变成"离开视口 200px 之后才取消"，
 * 语义开始需要额外解释；而监督墙的滚动是低频动作，省下的那点等待
 * 不值得让 §52 的策略变得难以验证。
 */
const defaultFactory: TileVisibilityFactory = (_studentId, onChange) => {
  let target: Element | null = null

  const observer = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        // 每张卡片一个 observer 时只会收到自己那条 entry；这层过滤是防止
        // 将来把多张卡片合并到一个 observer 时把可见性算到别人头上。
        if (entry.target !== target) continue
        onChange(entry.isIntersecting)
      }
    },
    { threshold: 0 },
  )

  return {
    observe(element: Element): void {
      target = element
      observer.observe(element)
    },
    disconnect(): void {
      observer.disconnect()
      target = null
    },
  }
}

let activeFactory: TileVisibilityFactory = defaultFactory

export function createTileVisibility(
  studentId: string,
  onChange: (visible: boolean) => void,
): TileVisibilityHandle {
  return activeFactory(studentId, onChange)
}

/** 注入替身工厂（仅供测试）。 */
export function setTileVisibilityFactory(factory: TileVisibilityFactory): void {
  activeFactory = factory
}

/** 还原真实工厂（测试的 afterEach 调用，避免污染下一个测试文件）。 */
export function resetTileVisibilityFactory(): void {
  activeFactory = defaultFactory
}
