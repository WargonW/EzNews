/**
 * HTTP 客户端 —— 鉴权、单飞刷新、错误归一化。
 *
 * ==================== ★ DEC-10 single-flight（实现强制项） ====================
 * Access Token 有效期 2h，并发多个请求同时过期是常态（列表 + 详情 + 音频）。
 * 若每个 401 各自发一次 `/auth/refresh`，服务端会看到**同一个 refresh token 的并发轮换**：
 *   - 第一发轮换成功，旧 hash 移入 prev_refresh_token_hash；
 *   - 第二发带的是**已作废**的旧串 → 命中 reuse 检测；
 *   - 若超出 60s 宽限窗口或 client_id 不匹配 → **撤销该用户全部 session +
 *     token_version+1** → 用户被自己的并发请求踢下线，且所有设备一起掉线。
 * 这就是 DEC-7 验收用例 T2 的回归红线。
 *
 * 实现：`refreshInFlight` 保存进行中的刷新 Promise。
 *   - 首个遇到 TOKEN_EXPIRED 的请求创建 Promise 并发起刷新；
 *   - 其余请求 await **同一个** Promise，拿到同一份新 token 后重放；
 *   - 刷新失败 → 统一 reject，降级游客态（不各自重试）。
 * 另外 `retryOnce` 标记保证**同一请求最多重放一次**，避免 refresh 返回的
 * token 仍被服务端判过期时形成无限循环。
 *
 * ============================ Cookie 通道 ============================
 * 所有请求都带 `credentials: 'include'`，让浏览器自动携带服务端下发的
 * httpOnly refresh Cookie（同源部署，DEC-12）。前端读不到也不需要读它。
 */

import type { Envelope, EnvelopeCode, TokenResponse } from './types';
import { useAuthStore, getClientId } from '@/stores/auth';

/** API base path。同源部署下用相对路径，由 vite proxy / 反代转发到 Go 服务端。 */
export const API_BASE = import.meta.env.VITE_API_BASE_URL ?? '';

/** 业务错误：携带 HTTP 状态码与 envelope code，供 UI 分支处理。 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: EnvelopeCode | 'NETWORK_ERROR';
  readonly requestId?: string;
  readonly details?: ReadonlyArray<{ field: string; message: string }>;

  constructor(
    status: number,
    code: EnvelopeCode | 'NETWORK_ERROR',
    message: string,
    requestId?: string,
    details?: ReadonlyArray<{ field: string; message: string }>,
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.requestId = requestId;
    this.details = details;
  }

  /** 是否为"token 过期、值得静默刷新并重放"的情形。 */
  get isTokenExpired(): boolean {
    return this.status === 401 && this.code === 'TOKEN_EXPIRED';
  }

  /** 是否为"凭证彻底失效、应降级游客态"的情形。 */
  get isUnauthorized(): boolean {
    return this.status === 401 && this.code !== 'TOKEN_EXPIRED';
  }

  /** 触发 refresh 时应尊重的退避秒数（429/503 的 Retry-After）。 */
  get retryAfterSec(): number | undefined {
    return (this as ApiError & { _retryAfter?: number })._retryAfter;
  }
}

/** 内部用于挂载 Retry-After 的辅助（不污染公开字段语义）。 */
export function withRetryAfter<T extends ApiError>(err: T, seconds: number | null): T {
  if (seconds !== null && Number.isFinite(seconds) && seconds > 0) {
    (err as T & { _retryAfter?: number })._retryAfter = seconds;
  }
  return err;
}

type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';

interface RequestOptions {
  method?: Method;
  /** 原始路径，如 `/api/v1/articles`。 */
  path: string;
  /** JSON 请求体；undefined 表示不带 body。 */
  body?: unknown;
  /** 查询参数；undefined / 空值的键会被剔除。 */
  query?: Record<string, string | number | boolean | undefined | null>;
  /** 期望返回 204（无响应体）。 */
  expectNoContent?: boolean;
  /** 内部使用：标记这是 401 后的重放，防止无限递归。 */
  _isRetry?: boolean;
  /** 跳过 Authorization 头（登录/注册/刷新端点自身不需要）。 */
  skipAuth?: boolean;
  /** 覆盖超时毫秒；音频文件等长请求可调大。 */
  timeoutMs?: number;
}

const DEFAULT_TIMEOUT_MS = 15_000;

/* ========================================================================== *
 * single-flight refresh
 * ========================================================================== */

/**
 * 进行中的刷新 Promise。
 *
 * ★ 必须挂在**模块作用域**而非组件/QueryClient 上：它的语义是
 *   "这个浏览器标签页此刻正在刷新"，跨所有调用方共享同一份。
 */
let refreshInFlight: Promise<string | null> | null = null;

/** 刷新失败后的退避截止时间戳（epoch ms）。失败后短窗口内不再重试刷新。 */
let refreshBackoffUntil = 0;

/** 退避窗口：refresh 失败（refresh 本身 401）后，等待这么久才允许再试。 */
const REFRESH_BACKOFF_MS = 30_000;

/** 是否正处于刷新中（供 UI 显示"正在恢复会话"，避免误判为游客态）。 */
export function isRefreshing(): boolean {
  return refreshInFlight !== null;
}

/**
 * 执行一次刷新，返回新的 accessToken；失败返回 null。
 *
 * 并发调用共享同一个 Promise —— 这就是 DEC-10 的全部实现。
 * 注意本函数**只发一次网络请求**，调用方无论多少个都 await 同一结果。
 */
async function performRefresh(): Promise<string | null> {
  if (refreshInFlight) return refreshInFlight;

  const run = async (): Promise<string | null> => {
    // 失败退避：避免 refresh 凭证已死时每个请求都去打一次（打满服务端限流）。
    if (Date.now() < refreshBackoffUntil) return null;

    try {
      // 直接 fetch 而不走 request()：避免递归依赖，且必须允许携带 Cookie。
      const res = await fetch(`${API_BASE}/api/v1/auth/refresh`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        // 同源 Cookie 部署下服务端从 Cookie 读 refreshToken，请求体可为空。
        // 仍带上内存中的 refreshToken 以兼容"跨域降级 / refreshTokenInBody=true"部署。
        body: JSON.stringify(buildRefreshBody()),
      });

      if (!res.ok) {
        // 401 = refresh 凭证失效（过期/撤销/reuse 处置），必须降级游客态。
        if (res.status === 401) refreshBackoffUntil = Date.now() + REFRESH_BACKOFF_MS;
        return null;
      }

      const json = (await res.json()) as Envelope<TokenResponse>;
      // Envelope.data 的契约类型是 `T | EnvelopeDetail[] | null`（错误响应放 details 数组），
      // 成功路径下服务端只会给 T，这里显式收窄。
      const token = json.data;
      if (!token || Array.isArray(token) || typeof token.accessToken !== 'string') return null;

      useAuthStore.getState().applyTokens(token);
      return token.accessToken;
    } catch {
      // 网络中断：不算凭证失效，不设退避（网络恢复后自然重试）。
      return null;
    }
  };

  refreshInFlight = run();

  try {
    return await refreshInFlight;
  } finally {
    // 无论成败都要清空，否则后续请求会永久复用已 settle 的 Promise。
    refreshInFlight = null;
  }
}

/**
 * 构造 refresh 请求体。
 *
 * ★ `clientId` **无条件发送**（永不为 undefined）：openapi `auth/refresh` 的
 *   requestBody 已声明 `clientId`，服务端 `dto_auth.go` 的 `RefreshRequest`
 *   也有 `ClientID` 字段。DEC-7 的宽限重放判定**依赖**它 ——
 *   同一 refresh token 在宽限窗口内再次出现时，服务端比对 `client_id`：
 *   相同 → 判为弱网重试，再轮换一次且不惩罚用户；
 *   不同/缺省 → 判为疑似泄露，撤销该用户**全部** session + `token_version+1`。
 *   一旦漏发，用户会被自己的弱网重试踢下线，且所有设备一起掉线。
 *
 * 同源 Cookie 部署下 refreshToken 由服务端从 Cookie 读，此处仅作跨域降级兜底。
 */
function buildRefreshBody(): { refreshToken?: string | null; clientId: string } {
  const body: { refreshToken?: string | null; clientId: string } = { clientId: getClientId() };
  const rt = useAuthStore.getState().refreshToken;
  if (rt) body.refreshToken = rt;
  return body;
}

/* ========================================================================== *
 * 响应解析
 * ========================================================================== */

/** 从可能含 details 的 data 中安全取出 details 数组。 */
function extractDetails(data: Envelope<unknown>['data']): ReadonlyArray<{ field: string; message: string }> | undefined {
  if (!Array.isArray(data)) return undefined;
  const details = data.filter(
    (d): d is { field: string; message: string } =>
      typeof d === 'object' &&
      d !== null &&
      typeof (d as { field?: unknown }).field === 'string' &&
      typeof (d as { message?: unknown }).message === 'string',
  );
  return details.length > 0 ? details : undefined;
}

/** 把任意 HTTP 响应转成 ApiError。 */
async function toApiError(res: Response): Promise<ApiError> {
  const retryAfterHeader = res.headers.get('Retry-After');
  const retryAfterSec = retryAfterHeader ? Number.parseInt(retryAfterHeader, 10) : null;

  let code: EnvelopeCode | 'NETWORK_ERROR' = 'INTERNAL_ERROR';
  let message = `请求失败（HTTP ${res.status}）`;
  let requestId: string | undefined;
  let details: ReadonlyArray<{ field: string; message: string }> | undefined;

  try {
    const text = await res.text();
    if (text) {
      const parsed = JSON.parse(text) as Partial<Envelope<unknown>>;
      if (typeof parsed.code === 'string') {
        code = parsed.code as EnvelopeCode;
      }
      if (typeof parsed.message === 'string' && parsed.message) {
        message = parsed.message;
      }
      if (typeof parsed.requestId === 'string') requestId = parsed.requestId;
      details = extractDetails(parsed.data ?? null);
    }
  } catch {
    // 响应体不是 JSON（如反代返回 HTML 502 页）：保留默认 message。
  }

  return withRetryAfter(new ApiError(res.status, code, message, requestId, details), retryAfterSec);
}

/** 带超时的 fetch 包装。 */
async function fetchWithTimeout(url: string, init: RequestInit, timeoutMs: number): Promise<Response> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    return await fetch(url, { ...init, signal: controller.signal });
  } finally {
    clearTimeout(timer);
  }
}

/** 构建查询串，跳过 undefined / null / 空串。 */
function buildQuery(query: RequestOptions['query']): string {
  if (!query) return '';
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '') continue;
    params.set(k, String(v));
  }
  const s = params.toString();
  return s ? `?${s}` : '';
}

/* ========================================================================== *
 * 核心请求
 * ========================================================================== */

/**
 * 发起一次 API 请求。
 *
 * 流程：构造请求 → 发送 → 若 401/TOKEN_EXPIRED 且未重放过 →
 *       单飞刷新 → 拿到新 token 重放一次 → 归一化返回值或抛 ApiError。
 */
export async function request<T>(options: RequestOptions): Promise<T> {
  const { method = 'GET', path, body, query, expectNoContent = false, timeoutMs = DEFAULT_TIMEOUT_MS } = options;

  const url = `${API_BASE}${path}${buildQuery(query)}`;

  const headers: Record<string, string> = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  // 业务接口是"可选鉴权"：带 token 时服务端额外返回 isFavorited/isRead，
  // 不带则按游客放行。所以游客态也走同一套 API，只是没有 Authorization 头。
  if (!options.skipAuth) {
    const token = useAuthStore.getState().accessToken;
    if (token) headers.Authorization = `Bearer ${token}`;
  }

  const init: RequestInit = {
    method,
    headers,
    // ★ 必须带 Cookie：refresh 的 httpOnly Cookie 只在带凭据时发送。
    credentials: 'include',
  };
  if (body !== undefined) init.body = JSON.stringify(body);

  let res: Response;
  try {
    res = await fetchWithTimeout(url, init, timeoutMs);
  } catch (err) {
    // 区分超时与网络不可达，统一为 NETWORK_ERROR（UI 据此展示离线态）。
    const aborted = err instanceof DOMException && err.name === 'AbortError';
    throw new ApiError(
      0,
      'NETWORK_ERROR',
      aborted ? '请求超时，请检查网络后重试' : '网络不可用，请检查网络连接',
    );
  }

  // ---- 401 处理：TOKEN_EXPIRED → 单飞刷新 + 重放 ----
  if (res.status === 401 && !options._isRetry) {
    const err = await toApiError(res);
    if (err.isTokenExpired) {
      const newToken = await performRefresh();
      if (newToken) {
        // ★ 复用同一个 nonce / 同一个 body 重放，保证幂等。
        return request<T>({ ...options, _isRetry: true });
      }
      // 刷新失败：降级游客态，抛原始错误由 UI 决定降级展示。
      useAuthStore.getState().degradeToGuest();
    }
    throw err;
  }

  // ---- 204 / 成功 ----
  if (expectNoContent || res.status === 204) {
    // 必须消费 body 以释放连接（fetch 的 body 未读会阻止复用）。
    void res.text().catch(() => undefined);
    return undefined as T;
  }

  if (!res.ok) throw await toApiError(res);

  // 304 Not Modified（文章详情/音频元数据带 ETag）——视为无新数据。
  if (res.status === 304) {
    void res.text().catch(() => undefined);
    return undefined as T;
  }

  const text = await res.text();
  if (!text) return undefined as T;

  let json: Envelope<T>;
  try {
    json = JSON.parse(text) as Envelope<T>;
  } catch {
    throw new ApiError(res.status, 'INTERNAL_ERROR', '响应不是合法 JSON');
  }

  // 服务端理论上成功时 code 恒为 OK；非 OK 一律视为错误（防御服务端违约）。
  if (json.code !== 'OK') {
    throw withRetryAfter(
      new ApiError(res.status, json.code, json.message || '请求失败', json.requestId, extractDetails(json.data ?? null)),
      res.headers.get('Retry-After') ? Number.parseInt(res.headers.get('Retry-After') as string, 10) : null,
    );
  }

  return json.data as T;
}

/** GET 便捷方法。 */
export function get<T>(path: string, query?: RequestOptions['query'], opts?: Partial<RequestOptions>): Promise<T> {
  return request<T>({ method: 'GET', path, query, ...opts });
}

/** POST 便捷方法。 */
export function post<T>(path: string, body?: unknown, opts?: Partial<RequestOptions>): Promise<T> {
  return request<T>({ method: 'POST', path, body, ...opts });
}

/** PUT 便捷方法。 */
export function put<T>(path: string, body?: unknown, opts?: Partial<RequestOptions>): Promise<T> {
  return request<T>({ method: 'PUT', path, body, ...opts });
}

/** PATCH 便捷方法。 */
export function patch<T>(path: string, body?: unknown, opts?: Partial<RequestOptions>): Promise<T> {
  return request<T>({ method: 'PATCH', path, body, ...opts });
}

/** DELETE 便捷方法（默认期望 204）。 */
export function del<T>(path: string, body?: unknown, opts?: Partial<RequestOptions>): Promise<T> {
  return request<T>({ method: 'DELETE', path, body, expectNoContent: true, ...opts });
}

/**
 * 静默恢复会话：应用启动 / 刷新页面后调用。
 *
 * 为什么要单独提供，而不是靠某个业务请求的 401 触发：
 * - 内存态被清空后发出的第一个请求**不带** Authorization 头，
 *   服务端会返回 `401 UNAUTHORIZED`（而非 `TOKEN_EXPIRED`），
 *   按 client 的策略是"直接降级游客态、**不刷新**" —— 于是会话永远恢复不了。
 * - 所以启动恢复必须**显式**打一次刷新。
 *
 * 复用同一个 `refreshInFlight`，因此若此时恰好有业务请求也在刷新，
 * 二者共享同一次网络请求（single-flight 依然成立）。
 *
 * @returns 恢复成功返回 true；失败（含无有效 refresh 凭证）返回 false。
 */
export async function restoreSession(): Promise<boolean> {
  const token = await performRefresh();
  return token !== null;
}

/** 仅供测试：重置 single-flight 状态。 */
export function __resetRefreshStateForTest(): void {
  refreshInFlight = null;
  refreshBackoffUntil = 0;
}