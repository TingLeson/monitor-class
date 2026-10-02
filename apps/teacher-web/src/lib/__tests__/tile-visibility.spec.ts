import { afterEach, describe, expect, it, vi } from 'vitest'
import { createTileVisibility } from '../tile-visibility.ts'

/**
 * 可见性观察的真实实现测试（§52）。
 *
 * 这一份刻意**不**注入替身工厂：它要钉住的正是生产路径本身——
 * 用 `threshold: 0` 观察卡片的根元素，并且只上报**自己那张卡片**的进出。
 * happy-dom 没有布局引擎（真实 IO 永远不会回调），所以这里替换掉全局的
 * `IntersectionObserver` 拿到回调，再手动喂 entry。
 */

class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = []

  readonly observed: Element[] = []
  disconnected = false

  constructor(
    readonly callback: IntersectionObserverCallback,
    readonly options?: IntersectionObserverInit,
  ) {
    FakeIntersectionObserver.instances.push(this)
  }

  observe(element: Element): void {
    this.observed.push(element)
  }

  unobserve(): void {
    // 本实现不使用 unobserve（disconnect 一次清干净）。
  }

  disconnect(): void {
    this.disconnected = true
  }

  takeRecords(): IntersectionObserverEntry[] {
    return []
  }
}

function lastObserver(): FakeIntersectionObserver {
  const observer = FakeIntersectionObserver.instances.at(-1)
  if (!observer) throw new Error('没有创建任何 IntersectionObserver')
  return observer
}

function emit(observer: FakeIntersectionObserver, target: Element, isIntersecting: boolean): void {
  observer.callback(
    [{ target, isIntersecting } as unknown as IntersectionObserverEntry],
    observer as unknown as IntersectionObserver,
  )
}

describe('卡片可见性（§52 的 IntersectionObserver）', () => {
  afterEach(() => {
    FakeIntersectionObserver.instances.length = 0
    vi.unstubAllGlobals()
  })

  it('用 threshold 0 观察卡片根元素，进/出视口都如实上报', () => {
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
    const element = document.createElement('div')
    const changes: boolean[] = []

    const handle = createTileVisibility('student-1', (visible) => changes.push(visible))
    handle.observe(element)

    const observer = lastObserver()
    // threshold 0：露出一角就算可见——露出一角的卡片也该开始拉流。
    expect(observer.options).toEqual({ threshold: 0 })
    expect(observer.observed).toEqual([element])

    emit(observer, element, true)
    emit(observer, element, false)
    expect(changes).toEqual([true, false])
  })

  it('别人家卡片的 entry 不会被算到自己头上（合并 observer 时也不会串台）', () => {
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
    const mine = document.createElement('div')
    const other = document.createElement('div')
    const changes: boolean[] = []

    const handle = createTileVisibility('student-1', (visible) => changes.push(visible))
    handle.observe(mine)

    emit(lastObserver(), other, true)
    expect(changes).toEqual([])
  })

  it('卸载时断开观察（不再持有元素，也不再有回调）', () => {
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
    const element = document.createElement('div')
    const changes: boolean[] = []

    const handle = createTileVisibility('student-1', (visible) => changes.push(visible))
    handle.observe(element)
    const observer = lastObserver()

    handle.disconnect()

    expect(observer.disconnected).toBe(true)
    // 断开之后再来的 entry 会被 target 过滤掉（target 已经清空）。
    emit(observer, element, true)
    expect(changes).toEqual([])
  })
})
