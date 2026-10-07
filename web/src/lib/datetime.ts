/**
 * 时间格式化工具。
 *
 * ★ 契约：API 返回 ISO 8601 UTC 字符串（`YYYY-MM-DDTHH:mm:ss.sssZ`），
 *   部分字段（MergeItem.updatedAt / StateItem.updatedAt / serverTimeMs /
 *   expiresIn 衍生）是 **epoch 毫秒数字**。本文件对两者分别处理，绝不混用。
 *
 * 展示一律按**浏览器本地时区**渲染（不硬编码 UTC+8）：
 * 部署者可能在任意时区，硬编码会让"2分钟前"变成错误的时间。
 */

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** 相对时间：刚刚 / N分钟前 / N小时前 / 昨天 / MM-DD / YYYY-MM-DD。 */
export function formatRelative(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';

  const diff = now - t;
  // 未来时间（服务端时钟超前 / 时区问题）显示为"刚刚"，不显示"负 N 分钟前"。
  if (diff < 0) return '刚刚';
  if (diff < MINUTE) return '刚刚';
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)} 分钟前`;
  if (diff < DAY) return `${Math.floor(diff / HOUR)} 小时前`;
  if (diff < 2 * DAY) return '昨天';
  if (diff < 7 * DAY) return `${Math.floor(diff / DAY)} 天前`;

  const d = new Date(t);
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  // 跨年才显示年份，减少视觉噪音。
  return y === new Date(now).getFullYear() ? `${m}-${day}` : `${y}-${m}-${day}`;
}

/** 绝对时间（本地时区）：YYYY-MM-DD HH:mm。 */
export function formatAbsolute(iso: string | null | undefined): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const d = new Date(t);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** 仅日期（本地时区）：YYYY-MM-DD。 */
export function formatDate(iso: string | null | undefined): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const d = new Date(t);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

/** 短时间（本地时区）：HH:mm。 */
export function formatTime(iso: string | null | undefined): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const d = new Date(t);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}`;
}

/* ========================================================================== *
 * epoch 秒（管理后台契约专用）
 * --------------------------------------------------------------------------
 * ★ 后台 DTO 的时间字段一律是 **epoch 秒整数**（`AdminUserDTO.createdAt` /
 *   `lastLoginAt`、`AdminAudioTaskDTO.createdAt` / `updatedAt`、
 *   `AdminSourceStatDTO.lastSeenAt`、`AdminOverviewDTO.generatedAt`、
 *   `AdminSystemDTO.startedAt`），与阅读端的 ISO 8601 字符串**不是一套**，
 *   也和 MergeItem/StateItem 的 **epoch 毫秒**不同 —— 三种口径不能混用。
 *   下面这组函数统一收口"秒"，避免每个后台页面各写一遍 `* 1000`。
 *
 * ★ 一律把 `<= 0` 视为"没有这个值"并返回空串：
 *   服务端对"从未登录"这类缺省返回 0（不是 null），按 0 渲染会得到
 *   "1970-01-01"，那是比留空更糟的错误信息。
 * ========================================================================== */

/** epoch 秒 → Date；非正值返回 null。 */
function dateFromUnixSec(sec: number | null | undefined): Date | null {
  if (sec === null || sec === undefined) return null;
  if (!Number.isFinite(sec) || sec <= 0) return null;
  return new Date(sec * 1000);
}

/** epoch 秒 → 相对时间（刚刚 / N 分钟前 / …）；无效值返回空串。 */
export function formatRelativeSec(sec: number | null | undefined, now = Date.now()): string {
  const d = dateFromUnixSec(sec);
  if (!d) return '';
  return formatRelative(d.toISOString(), now);
}

/** epoch 秒 → 绝对时间（本地时区）YYYY-MM-DD HH:mm；无效值返回空串。 */
export function formatAbsoluteSec(sec: number | null | undefined): string {
  const d = dateFromUnixSec(sec);
  if (!d) return '';
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/**
 * epoch 秒时长 → 人类可读的运行时长（用于服务端 uptime）。
 *
 * 与时长不同的地方：uptime 的量级是"天"，粒度到秒没有意义，
 * 这里只保留 天/小时/分钟 三级。
 */
export function formatUptime(sec: number | null | undefined): string {
  if (sec === null || sec === undefined || !Number.isFinite(sec) || sec < 0) return '';
  const totalMin = Math.floor(sec / 60);
  if (totalMin < 1) return '不到 1 分钟';
  if (totalMin < 60) return `${totalMin} 分钟`;
  const hours = Math.floor(totalMin / 60);
  if (hours < 24) return `${hours} 小时 ${totalMin % 60} 分钟`;
  return `${Math.floor(hours / 24)} 天 ${hours % 24} 小时`;
}

/** 毫秒时长 → mm:ss（音频进度用）。 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '0:00';
  const totalSec = Math.floor(ms / 1000);
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}

/** 字节数 → 人类可读。 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  let v = bytes;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

/** epoch 毫秒 → 本地 ISO 串（调试与日志用）。 */
export function formatEpochMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '';
  return new Date(ms).toISOString();
}

/** 估算中文文本的朗读时长（ms），用于播放前预估。 */
export function estimateSpeechMs(text: string, speed = 1): number {
  const chars = text.replace(/\s+/g, '').length;
  // 中文播报约 5 字/秒（speed=1），英文按字符数折算。
  const baseMs = (chars / 5) * 1000;
  const adjusted = speed > 0 ? baseMs / speed : baseMs;
  return Math.max(1000, Math.round(adjusted));
}