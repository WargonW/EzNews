/**
 * 认证状态（zustand）。
 *
 * ============================ 安全不变量 ============================
 * 1. **accessToken 只存内存**。绝不写 localStorage —— XSS 可读持久化存储，
 *    内存态至少活不过一次刷新。这是 DEC-12 / PRD-ACCOUNT §3.3 的硬性要求。
 * 2. **refreshToken 前端永不持有**（同源部署下由服务端写 httpOnly Cookie，
 *    JS 读不到也不需要读）。若服务端 `refreshTokenInBody=true` 会在响应体带回，
 *    此时也**只在内存持有**，不持久化（"通道可以开两条，持久化路径只能有一条"）。
 * 3. 刷新页面后靠 `POST /api/v1/auth/refresh`（Cookie 自动携带）静默恢复会话。
 */

import { create } from 'zustand';
import type { TokenResponse, UserDTO } from '@/api/types';
import {
  StorageKey,
  randomId,
  readString,
  removeKey,
  writeString,
} from '@/lib/storage';

/** 会话状态机。 */
export type AuthStatus =
  /** 尚未尝试恢复（首屏一瞬），此时不渲染任何"请登录"提示，避免闪烁。 */
  | 'initializing'
  /** 游客态：可完整浏览。 */
  | 'guest'
  /** 已登录。 */
  | 'authenticated'
  /** 静默刷新失败（refresh 过期/撤销），已降级游客态。 */
  | 'expired';

interface AuthState {
  status: AuthStatus;
  /** 仅内存。 */
  accessToken: string | null;
  /** 仅内存。跨域降级部署下服务端会下发；同源 Cookie 部署下恒为 null。 */
  refreshToken: string | null;
  /** accessToken 过期时刻（epoch ms），用于提前刷新与判断是否需要 refresh。 */
  expiresAtMs: number;
  user: UserDTO | null;

  /** 登录/注册成功后写入内存态（不落盘）。 */
  applyTokens: (t: TokenResponse) => void;
  /** 单独更新账号信息（如从 `GET /me` 拉取到最新资料）。 */
  setUser: (u: UserDTO) => void;
  /** 静默恢复失败 → 降级游客态并清空内存凭证。 */
  degradeToGuest: () => void;
  /** 登出（本地部分）：清空内存凭证，回到游客态。 */
  clearLocalSession: () => void;
  /** 设置初始化完成（无论成功与否）。 */
  markInitialized: () => void;
  /** 读取内存中的 accessToken（供 api 层使用，避免直接依赖组件）。 */
  getAccessToken: () => string | null;
}

/**
 * 客户端实例标识（clientId）。
 *
 * 用途有二，都很关键：
 * 1. `MergeRequest.clientId` —— merge 幂等键的一半。
 * 2. `POST /auth/refresh` 的 `clientId` —— **DEC-7 前置契约**：宽限重放判定
 *    必须比对 client_id，不带则服务端只能按疑似泄露处置，宽限期等于不存在。
 *
 * 持久化在 localStorage（它是客户端实例标识，不是秘密——PRD 明示）。
 */
export function getClientId(): string {
  const existing = readString(StorageKey.clientId);
  if (existing && existing.length > 0 && existing.length <= 64) return existing;
  const fresh = randomId(24).slice(0, 24);
  writeString(StorageKey.clientId, fresh);
  return fresh;
}

/** 默认设备名（session.device_name），用于会话列表可读性。 */
export function getDeviceName(): string {
  const ua = typeof navigator !== 'undefined' ? navigator.userAgent : '';
  const platformHint =
typeof navigator !== 'undefined' && 'userAgentData' in navigator
      ? ((navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform ?? '')
      : '';

  let browser = 'Browser';
  if (/Edg\//.test(ua)) browser = 'Edge';
  else if (/OPR\//.test(ua)) browser = 'Opera';
  else if (/Firefox\//.test(ua)) browser = 'Firefox';
  else if (/Chrome\//.test(ua)) browser = 'Chrome';
  else if (/Safari\//.test(ua)) browser = 'Safari';

  const os = /Windows/.test(platformHint)
    ? 'Windows'
    : /Android/.test(platformHint)
      ? 'Android'
      : /iPhone|iPad|iPod/.test(platformHint)
        ? 'iOS'
        : /Mac/.test(platformHint)
          ? 'macOS'
          : /Linux/.test(platformHint)
            ? 'Linux'
            : /Windows/.test(ua)
              ? 'Windows'
              : /Android/.test(ua)
                ? 'Android'
                : /iPhone|iPad|iPod/.test(ua)
                  ? 'iOS'
                  : /Mac/.test(ua)
                    ? 'macOS'
                    : /Linux/.test(ua)
                      ? 'Linux'
                      : 'Unknown';

  return `${browser} on ${os}`.slice(0, 64);
}

export const useAuthStore = create<AuthState>((set, get) => ({
  status: 'initializing',
  accessToken: null,
  refreshToken: null,
  expiresAtMs: 0,
  user: null,

  applyTokens: (t) => {
    // ★ 这里只把 token 放进内存（zustand state），**刻意不调 assertNoCredentialLeak**。
    //   凭证护栏守在 storage.writeString —— 那是唯一真正写 localStorage 的函数
    //   （writeJson 也走它，自动继承）。
    //   护栏曾错位放在这里，导致开发模式下登录 100% 失败：accessToken 是三段点分 JWT，
    //   护栏对任意 JWT 都 throw，而 AuthDialog 把它归类成「网络异常，请检查连接后重试」——
    //   一个纯内存操作被持久化护栏拦下，密码正确也登不进去，且错误信息完全指错方向。
    set({
      status: 'authenticated',
      accessToken: t.accessToken,
      // 同源 Cookie 部署下服务端返回 null；跨域降级时响应体带下来。
      // 两种情况都只在内存持有，绝不持久化。
      refreshToken: t.refreshToken ?? null,
      expiresAtMs: Date.now() + Math.max(0, t.expiresIn) * 1000,
      user: t.user,
    });
  },

  setUser: (u) => set({ user: u }),

  degradeToGuest: () => {
    set({ status: 'expired', accessToken: null, refreshToken: null, expiresAtMs: 0, user: null });
  },

  clearLocalSession: () => {
    // 登出后清掉与该账号关联的同步快照，避免跨账号串数据（Merge Policy §4.2）。
    removeKey(StorageKey.favoriteSyncCursor);
    removeKey(StorageKey.readSyncCursor);
    removeKey(StorageKey.mergeNonce);
    set({ status: 'guest', accessToken: null, refreshToken: null, expiresAtMs: 0, user: null });
  },

  markInitialized: () => {
    if (get().status === 'initializing') set({ status: 'guest' });
  },

  getAccessToken: () => get().accessToken,
}));

/** 是否已登录的便捷选择器。 */
export const selectIsAuthenticated = (s: AuthState): boolean =>
  s.status === 'authenticated' && s.accessToken !== null;

/** 是否已完成首次会话恢复（用于避免首屏误显示游客态 UI）。 */
export const selectAuthReady = (s: AuthState): boolean => s.status !== 'initializing';