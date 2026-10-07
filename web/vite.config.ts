/// <reference types="vitest" />
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';

/**
 * Vite 配置。
 *
 * 开发期通过 server.proxy 把 /api、/healthz、/readyz、/metrics 代理到
 * http://127.0.0.1:8080（Go 服务端），这样浏览器侧一律使用同源相对路径
 * `/api/v1/...`，开发期不产生跨域，refresh 的 httpOnly Cookie 也能正常带上。
 *
 * 关键：Cookie 是同源通道。若前端直接指向 http://127.0.0.1:8080，
 * 浏览器会把请求视为跨域，SameSite=Lax 的 refresh Cookie 不会发送，
 * 静默刷新会直接失效——这正是 DEC-12「仅支持同源部署」的技术根因之一。
 */
const SERVER_ORIGIN = process.env.EZNEWS_SERVER_ORIGIN ?? 'http://127.0.0.1:8080';

const PROXY_TARGETS = ['/api', '/healthz', '/readyz', '/metrics'];

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: Object.fromEntries(
      PROXY_TARGETS.map((p) => [
        p,
        {
          target: SERVER_ORIGIN,
          changeOrigin: false,
          // Web 官方仅支持同源部署；这里保留原 Host 以便服务端按需判断。
          ws: false,
        },
      ]),
    ),
  },
  build: {
    target: 'es2022',
    sourcemap: false,
    rollupOptions: {
      output: {
        // 路由级懒加载的产物分离到独立 chunk，避免首屏加载全部页面。
        manualChunks: {
          react: ['react', 'react-dom', 'react-router-dom'],
          query: ['@tanstack/react-query'],
        },
      },
    },
  },

  /**
   * ============================ Vitest ============================
   *
   * 与 Vite 共用同一份配置（同一套 alias / 插件），所以测试里
   * `import ... from '@/stores/guest'` 走的是和生产构建**完全相同**的
   * 解析路径 —— 不会出现"测试能过、构建解析不到"这类假绿。
   *
   * 为什么这里没把 vitest 拆到独立文件：本项目的 alias 与 plugins
   * 都很少，且拆开后 alias 要复制一份，改一处忘另一处就会静默漂移。
   *
   * ★ include 刻意写成 glob 白名单，而不是用 exclude 反向排除：
   *   白名单下"新增测试忘了配include"会**直接跑不到用例**，CI 因
   *   "No test files found" 而失败；反向排除则会悄悄把新测试漏掉 ——
   *   后者正是本轮要堵的那类"改坏了没人拦"的洞，配置层面不能再留一个。
   */
  test: {
    environment: 'jsdom',
    globals: false,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
    // 生产源码目录全在 src 下，排除掉以防将来有人在 src 里放
    // 名字带 test 的普通模块（如 testUtils.ts）被误当测试收集。
    exclude: ['node_modules/**', 'dist/**'],
    // jsdom 下的 fetch/定时器偶发慢，给足超时避免 CI 上偶发红。
    testTimeout: 10_000,

    /**
     * ★ 串行跑测试文件。
     *
     * 起因是一个**实测到的**失败：并行时 vitest 的多个 worker 会同时写
     * 同一份 vite-node 模块缓存（`os.tmpdir()/<projectId>/web/<sha1>`），
     * 第二个写入者拿到 EPERM，表现为"用例全绿但 vitest 退出码 1"
     * —— 这在 CI 里就是一句莫名其妙的红灯，且**偶发**，极难定位。
     *
     * 为什么接受串行：当前只有 2 个测试文件、53 个用例，串行的总耗时与
     * 并行几乎无差（本机实测并行 2.1s / 串行 2.2s）。等测试文件多到
     * 串行明显拖慢 CI 时，再改成"给缓存目录换个位置"或升级 vitest，
     * 而不是现在就赌一个偶发红灯。**先要绿，再要快。**
     */
    fileParallelism: false,

    coverage: {
      provider: 'v8',
      // 放node_modules/.tmp：覆盖率是**产物**不是源码，不该出现在工程根目录
      // 被误提交；放这里也顺带避开根目录的清理动作。
      reportsDirectory: './node_modules/.tmp/coverage',
      // 只统计真正被测的两处逻辑，避免"覆盖率数字很好看"的假信号。
      include: ['src/stores/guest.ts', 'src/hooks/useMarkAllRead.ts'],
    },
  },
});