/**
 * Vitest 全局 setup（所有测试文件共用）。
 *
 * ============================ 这里只做"环境对齐"，不放测试逻辑 ============================
 * 三类事情：
 * 1. jsdom 缺的真实浏览器 API 补齐（crypto.randomUUID）；
 * 2. 每个测试之间清干净持久化状态，避免**测试间互相污染**
 *    （guest store 是模块级 zustand 单例，不清会跨用例串状态）；
 * 3. 兜底断言：环境配错时立刻炸，而不是每个用例各自报玄学错误。
 *
 * ★ 反面教材警示：不要在这里 `vi.mock` 业务模块。业务模块一旦被全局 mock，
 *   测试就变成"验证 mock 的行为"，改坏生产代码它照样绿 —— 那正是本轮要堵的漏。
 *   需要 mock 的东西一律在**各自的测试文件里** mock。
 */

import { beforeEach } from 'vitest';

/* ── 1. jsdom 缺失的浏览器 API ─────────────────────────────────────────── */

// guest store 在模块顶层就调 localStorage 探测。环境不是 jsdom 时，
// 症状会表现为"reads 恒为 {}"这种看不出根因的诡异失败，所以在这里硬断言。
if (typeof window === 'undefined' || typeof window.localStorage === 'undefined') {
  throw new Error('setup.ts: localStorage 不可用 —— vitest environment 未设为 jsdom');
}

/**
 * `crypto.randomUUID`：lib/storage.randomId 优先用它。
 *
 * jsdom 的 `crypto` 只实现了 getRandomValues，缺 randomUUID，
 * 会让 randomId 静默退化到 getRandomValues —— 结果虽可用，
 * 但测试环境与真实浏览器的分支不一致，属于"看不见的偏差"，补上。
 */
if (typeof globalThis.crypto === 'undefined') {
  throw new Error('setup.ts: 缺少 crypto —— jsdom 环境不完整');
}
if (typeof globalThis.crypto.randomUUID !== 'function') {
  Object.defineProperty(globalThis.crypto, 'randomUUID', {
    configurable: true,
    writable: true,
    value: (): `${string}-${string}-${string}-${string}-${string}` => {
      const bytes = new Uint8Array(16);
      globalThis.crypto.getRandomValues(bytes);
      // 强制 RFC4122 v4 的 version / variant 位，避免生成非规范 UUID。
      bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40;
      bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80;
      const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
      return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
    },
  });
}

/* ── 2. 测试间隔离 ─────────────────────────────────────────────────────── */

/**
 * 每个用例前清空 localStorage。
 *
 * guest / auth 两个 store 都在**模块加载时**读一次 localStorage，
 * 所以仅清盘还不够 —— store 内存里的值也得由各测试自行 reset（见 stores/__tests__）。
 * 这里清盘是为了让"下次模块加载"看到干净环境。
 */
beforeEach(() => {
  window.localStorage.clear();
});