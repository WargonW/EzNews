/// <reference types="vite/client" />

/**
 * Vite 注入的环境变量类型。
 *
 * 目前只有一个自定义变量：`VITE_API_BASE_URL`。
 *
 * ★ 默认为空字符串是**刻意的**：DEC-12 规定官方唯一支持的部署形态是**同源**
 *   （前端静态资源与 /api/v1 由同一个域名提供）。此时浏览器用相对路径请求，
 *   HttpOnly 的 refresh Cookie 才能正常携带。
 *   开发时由 vite.config.ts 的 server.proxy 转发到本地后端，也走相对路径。
 */
interface ImportMetaEnv {
  readonly VITE_API_BASE_URL?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}