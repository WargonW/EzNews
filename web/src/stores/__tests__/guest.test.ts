/**
 * `src/stores/guest.ts` 的回归网。
 *
 * ============================ 为什么只测这一个 store ============================
 * 它承载游客态收藏/已读的**墓碑 + LWW** 语义，是全项目最容易写出
 * "用户看不见但永远修不好的坏状态"的地方（本地 updatedAt 一旦比服务端新，
 * 服务端怎么改都同步不下来，且没有任何报错）。
 *
 * 具体覆盖三块：
 * 1. `applyCloudState` 的 LWW 取舍规则（含墓碑胜出、时钟钳制）；
 * 2. `markAllRead` 的快照语义（只含真正变化的键）；
 * 3. `rollbackReads` 是否**真的**还原（这是"乐观更新失败后不留假状态"的底座）。
 *
 * ★ 每个用例都断言到**具体值**而不是"不为空"。断言松一档，
 *   回归网就漏一个 bug；本文件里刻意用 toBe / toEqual 而非 toBeTruthy。
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { StateItem } from '@/api/types';
import { StorageKey } from '@/lib/storage';
import { DEFAULT_PREFERENCES, clampClientTimestamp, prefIdSet, serializeIdSet, useGuestStore } from '@/stores/guest';

/** 固定一个"当前时刻"，让 updatedAt 的断言可复现（不依赖真实时钟）。 */
const NOW = 1_700_000_000_000;

function resetStore(): void {
  useGuestStore.setState({
    favorites: {},
    reads: {},
    preferences: { ...DEFAULT_PREFERENCES },
  });
}

/** 直接写 reads，绕过 toggle 以便构造任意（含墓碑的）历史状态。 */
function seedReads(map: Record<number, { updatedAt: number; deleted: boolean }>): void {
  useGuestStore.setState({ reads: map });
}

function stateItem(articleId: number, updatedAt: number, deleted: boolean): StateItem {
  return { articleId, updatedAt, deleted };
}

/** 从 localStorage 读回游客已读的持久化形态（验证"内存改了，盘上也改了"）。 */
function persistedReads(): unknown {
  const raw = window.localStorage.getItem(StorageKey.guestReads);
  return raw === null ? null : JSON.parse(raw);
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
  resetStore();
});

afterEach(() => {
  vi.useRealTimers();
});

/* ========================================================================== *
 * clampClientTimestamp —— 钳制客户端时钟（"假状态永久保留"的根因防线）
 * ========================================================================== */

describe('clampClientTimestamp', () => {
  it('正常的当前时刻原样返回', () => {
    expect(clampClientTimestamp(NOW - 1000, NOW)).toBe(NOW - 1000);
  });

  it('updatedAt <= 0 压到 now（否则它会被 LWW 当成"最旧"而被云端无条件覆盖）', () => {
    expect(clampClientTimestamp(0, NOW)).toBe(NOW);
    expect(clampClientTimestamp(-1, NOW)).toBe(NOW);
  });

  it('NaN / Infinity 压到 now', () => {
    expect(clampClientTimestamp(Number.NaN, NOW)).toBe(NOW);
    expect(clampClientTimestamp(Number.POSITIVE_INFINITY, NOW)).toBe(NOW);
  });

  it('超前超过 7 天的时钟不准值压到 now —— 这条是"永久坏状态"的直接防线', () => {
    const skewed = NOW + 8 * 24 * 60 * 60 * 1000;
    expect(clampClientTimestamp(skewed, NOW)).toBe(NOW);
  });

  it('恰好 7 天边界（未超过）不钳制', () => {
    const edge = NOW + 7 * 24 * 60 * 60 * 1000;
    expect(clampClientTimestamp(edge, NOW)).toBe(edge);
  });
});

/* ========================================================================== *
 * applyCloudState —— LWW 合并的取舍规则
 * ========================================================================== */

describe('applyCloudState 的 LWW 合并', () => {
  it('云端 updatedAt 更新 → 采用云端', () => {
    seedReads({ 1: { updatedAt: NOW - 5_000, deleted: true } });

    useGuestStore.getState().applyCloudState([], [stateItem(1, NOW - 1_000, false)], {});

    expect(useGuestStore.getState().reads[1]).toEqual({ updatedAt: NOW - 1_000, deleted: false });
    expect(useGuestStore.getState().isRead(1)).toBe(true);
  });

  it('本地 updatedAt 更新 → 保留本地，云端不得覆盖（游客离线期间的合法操作）', () => {
    seedReads({ 1: { updatedAt: NOW - 1_000, deleted: false } });

    useGuestStore.getState().applyCloudState([], [stateItem(1, NOW - 5_000, true)], {});

    // 云端那条是墓碑且更新，但它更旧 → 本地"已读"必须活下来。
    expect(useGuestStore.getState().reads[1]).toEqual({ updatedAt: NOW - 1_000, deleted: false });
    expect(useGuestStore.getState().isRead(1)).toBe(true);
  });

  it('updatedAt 完全相等 → 采用云端（同一毫秒以云端为准，保证与服务端 LWW 一致）', () => {
    seedReads({ 1: { updatedAt: NOW, deleted: false } });

    useGuestStore.getState().applyCloudState([], [stateItem(1, NOW, true)], {});

    // 判定条件是 `cur.updatedAt > remoteTs` 才保留本地，所以相等时走云端分支。
    expect(useGuestStore.getState().reads[1]).toBeUndefined();
  });

  it('云端墓碑胜出 → 本地条目被移除（不是写成 deleted=true 的墓碑）', () => {
    seedReads({ 7: { updatedAt: NOW - 1_000, deleted: false } });

    useGuestStore.getState().applyCloudState([], [stateItem(7, NOW, true)], {});

    // DEC-11：墓碑胜出后本地**移除**该条，继续留墓碑会让它被原样上传成云端垃圾。
    expect(useGuestStore.getState().reads[7]).toBeUndefined();
    expect('7' in useGuestStore.getState().reads).toBe(false);
  });

  it('非法 articleId（0 / 负数 / 非整数）被跳过，不会污染本地 map', () => {
    seedReads({});

    useGuestStore
      .getState()
      .applyCloudState([], [stateItem(0, NOW, false), stateItem(-3, NOW, false), stateItem(1.5, NOW, false)], {});

    expect(useGuestStore.getState().reads).toEqual({});
  });

  it('云端时间戳超前超 7 天 → 先钳制再比LWW，不会伪造"本地最旧"', () => {
    seedReads({ 1: { updatedAt: NOW - 10_000, deleted: true } });

    // 服务端（或被改坏的客户端）送来 30 天后的时间戳；钳制后等于 NOW，
    // 仍比本地 NOW-10000 新 → 采用云端，且落盘的时间戳是钳制值而非原值。
    const skewed = NOW + 30 * 24 * 60 * 60 * 1000;
    useGuestStore.getState().applyCloudState([], [stateItem(1, skewed, false)], {});

    expect(useGuestStore.getState().reads[1]).toEqual({ updatedAt: NOW, deleted: false });
  });

  it('合并结果同时落到内存与 localStorage（跨刷新不丢）', () => {
    seedReads({});

    useGuestStore.getState().applyCloudState([], [stateItem(5, NOW, false)], {});

    expect(persistedReads()).toEqual({ '5': { updatedAt: NOW, deleted: false } });
  });

  it('favorites 与 reads 各自独立合并，互不串（曾把两类塞进同一map 的错法）', () => {
    useGuestStore.setState({
      favorites: { 3: { updatedAt: NOW - 1_000, deleted: false } },
      reads: { 4: { updatedAt: NOW - 1_000, deleted: false } },
    });

    // 只给云端 favorites，reads 必须原封不动。
    useGuestStore.getState().applyCloudState([stateItem(9, NOW, false)], [], {});

    expect(useGuestStore.getState().favorites[9]).toEqual({ updatedAt: NOW, deleted: false });
    expect(useGuestStore.getState().reads[4]).toEqual({ updatedAt: NOW - 1_000, deleted: false });
    expect(useGuestStore.getState().reads[9]).toBeUndefined();
  });

  it('偏好按 KV 合并：云端值覆盖同名键，本地独有键保留', () => {
    useGuestStore.setState({ preferences: { ...DEFAULT_PREFERENCES, 'ui.density': 'compact', 'tts.voice': '999' } });

    useGuestStore.getState().applyCloudState([], [], { 'ui.density': 'comfortable' });

    expect(useGuestStore.getState().preferences['ui.density']).toBe('comfortable');
    expect(useGuestStore.getState().preferences['tts.voice']).toBe('999');
  });
});

/* ========================================================================== *
 * markAllRead —— 快照语义
 * ========================================================================== */

describe('markAllRead 的快照', () => {
  it('快照只含真正变化的键：改动前不存在的键记为 undefined', () => {
    seedReads({});

    const snapshot = useGuestStore.getState().markAllRead([1, 2, 3]);

    expect(snapshot).toEqual({ 1: undefined, 2: undefined, 3: undefined });
    // ★ 键存在且值为 undefined：必须用 in 判定，"键不存在"与"值为 undefined"语义不同。
    expect('1' in snapshot).toBe(true);
  });

  it('已是已读的条目被跳过：不进快照，也不刷新它的 updatedAt', () => {
    seedReads({ 1: { updatedAt: NOW - 60_000, deleted: false } });

    const snapshot = useGuestStore.getState().markAllRead([1, 2]);

    // 1 不该出现（无意义地推进 updatedAt 会让下次增量同步白跑一趟）。
    expect('1' in snapshot).toBe(false);
    expect(useGuestStore.getState().reads[1]).toEqual({ updatedAt: NOW - 60_000, deleted: false });
    expect(snapshot[2]).toBeUndefined();
  });

  it('已是墓碑的条目：快照记下改动前的墓碑，回滚时才能写回墓碑而非删键', () => {
    const tombstone = { updatedAt: NOW - 30_000, deleted: true };
    seedReads({ 4: tombstone });

    const snapshot = useGuestStore.getState().markAllRead([4]);

    expect(snapshot[4]).toEqual(tombstone);
    expect(useGuestStore.getState().reads[4]).toEqual({ updatedAt: NOW, deleted: false });
  });

  it('非法 id（0 / 负数 / 非整数）被丢弃，既不入快照也不入 map', () => {
    seedReads({});

    const snapshot = useGuestStore.getState().markAllRead([0, -1, 2.5, 8]);

    expect(Object.keys(snapshot)).toEqual(['8']);
    expect(Object.keys(useGuestStore.getState().reads)).toEqual(['8']);
  });

  it('全部已读时返回空快照且不写盘（无变化就不该产生 I/O）', () => {
    seedReads({ 1: { updatedAt: NOW, deleted: false } });

    const snapshot = useGuestStore.getState().markAllRead([1]);

    expect(snapshot).toEqual({});
    expect(persistedReads()).toBeNull();
  });

  it('重复 id 只处理一次，快照只有一个键', () => {
    seedReads({});

    const snapshot = useGuestStore.getState().markAllRead([6, 6, 6]);

    expect(Object.keys(snapshot)).toEqual(['6']);
  });

  it('写入的 updatedAt 是钳制后的 now（本地时钟超前也不会伪造最新）', () => {
    vi.setSystemTime(NOW + 30 * 24 * 60 * 60 * 1000); // 模拟本机时钟跑飞 30 天
    seedReads({});

    useGuestStore.getState().markAllRead([1]);

    // Date.now() 本身就是"错的现在"，clampClientTimestamp 拿它当基准，
    // 结果 updatedAt 仍等于 now-30d 而非某个更荒谬的值。
    expect(useGuestStore.getState().reads[1]).toEqual({ updatedAt: NOW + 30 * 24 * 60 * 60 * 1000, deleted: false });
  });
});

/* ========================================================================== *
 * rollbackReads —— 是否真的还原（乐观更新失败后不留假状态）
 * ========================================================================== */

describe('rollbackReads', () => {
  it('改动前不存在的键 → 回滚后删键，而不是凭空造出墓碑', () => {
    seedReads({});
    const before = { ...useGuestStore.getState().reads };

    const snapshot = useGuestStore.getState().markAllRead([1, 2]);
    expect(useGuestStore.getState().reads[1]).toBeDefined();

    useGuestStore.getState().rollbackReads(snapshot);

    //★ 这是最容易写错的一处：写成 deleted=true 会让它以墓碑形式上传云端。
    expect(useGuestStore.getState().reads).toEqual(before);
    expect('1' in useGuestStore.getState().reads).toBe(false);
    expect(useGuestStore.getState().toMergeItems('reads')).toEqual([]);
  });

  it('改动前是墓碑的键 → 回滚后原样写回墓碑（不是删键）', () => {
    const tombstone = { updatedAt: NOW - 30_000, deleted: true };
    seedReads({ 4: tombstone });
    const before = { ...useGuestStore.getState().reads };

    const snapshot = useGuestStore.getState().markAllRead([4]);
    useGuestStore.getState().rollbackReads(snapshot);

    expect(useGuestStore.getState().reads[4]).toEqual(tombstone);
    expect(useGuestStore.getState().reads).toEqual(before);
  });

  it('完整还原：markAllRead → rollbackReads 后 reads 深度等于改动前', () => {
    seedReads({
      1: { updatedAt: NOW - 60_000, deleted: true }, // 墓碑
      2: { updatedAt: NOW - 60_000, deleted: false }, // 已读（会被跳过）
      3: { updatedAt: NOW - 60_000, deleted: false }, // 未改动（不在传入 ids 里）
    });
    const before = structuredClone(useGuestStore.getState().reads);

    const snapshot = useGuestStore.getState().markAllRead([1, 2, 3, 4]);
    useGuestStore.getState().rollbackReads(snapshot);

    expect(useGuestStore.getState().reads).toEqual(before);
  });

  it('回滚只碰快照里的键，不影响期间其它操作写入的键', () => {
    const tombstone = { updatedAt: NOW - 1_000, deleted: true };
    seedReads({ 1: tombstone });

    const snapshot = useGuestStore.getState().markAllRead([1, 2]);
    // 模拟"提交期间用户又手动标了 99"。
    useGuestStore.setState((s) => ({ reads: { ...s.reads, 99: { updatedAt: NOW, deleted: false } } }));

    useGuestStore.getState().rollbackReads(snapshot);

    // 99 不在快照里 → 必须活着。
    expect(useGuestStore.getState().reads[99]).toEqual({ updatedAt: NOW, deleted: false });
    // 1 改动前是墓碑 → 写回墓碑（不是删键）。
    expect(useGuestStore.getState().reads[1]).toEqual(tombstone);
    // 2 改动前不存在 → 删键。
    expect('2' in useGuestStore.getState().reads).toBe(false);
  });

  it('回滚同样落盘：刷新页面不会看到已回滚的假状态', () => {
    seedReads({});

    const snapshot = useGuestStore.getState().markAllRead([1]);
    useGuestStore.getState().rollbackReads(snapshot);

    expect(persistedReads()).toEqual({});
  });

  it('空快照回滚是安全的 no-op（不改引用语义）', () => {
    seedReads({ 1: { updatedAt: NOW, deleted: false } });

    useGuestStore.getState().rollbackReads({});

    expect(useGuestStore.getState().reads).toEqual({ 1: { updatedAt: NOW, deleted: false } });
  });
});

/* ========================================================================== *
 * 游客态 ↔ 登录态交叉：本地已读要能上传，且回滚后不能残留
 * ========================================================================== */

describe('游客态与登录态交叉时的已读往返', () => {
  it('游客标记已读 → toMergeItems 能把它带给云端', () => {
    seedReads({});

    useGuestStore.getState().markAllRead([11, 12]);

    const items = useGuestStore.getState().toMergeItems('reads');
    expect(items).toHaveLength(2);
    expect(items.every((i) => i.deleted === false && i.updatedAt === NOW)).toBe(true);
    expect(items.map((i) => i.articleId).sort((a, b) => a - b)).toEqual([11, 12]);
  });

  it('失败回滚后 toMergeItems 不再包含这批 —— 否则假状态会被上传且无法自愈', () => {
    seedReads({});

    const snapshot = useGuestStore.getState().markAllRead([11, 12]);
    useGuestStore.getState().rollbackReads(snapshot);

    expect(useGuestStore.getState().toMergeItems('reads')).toEqual([]);
  });

  it('游客标记 → 服务端原样回声（updatedAt 相同）→ 仍是已读', () => {
    seedReads({});

    useGuestStore.getState().markAllRead([11]);
    const echoed = useGuestStore.getState().toMergeItems('reads').map((i) => ({ ...i }));

    useGuestStore.getState().applyCloudState([], echoed, {});

    expect(useGuestStore.getState().isRead(11)).toBe(true);
  });

  it('resetGuestState 清空收藏/已读并回到默认偏好（合并成功后避免重复上传）', () => {
    seedReads({ 1: { updatedAt: NOW, deleted: false } });
    useGuestStore.setState({ favorites: { 2: { updatedAt: NOW, deleted: false } } });

    useGuestStore.getState().resetGuestState();

    expect(useGuestStore.getState().reads).toEqual({});
    expect(useGuestStore.getState().favorites).toEqual({});
    expect(useGuestStore.getState().preferences).toEqual(DEFAULT_PREFERENCES);
    expect(window.localStorage.getItem(StorageKey.guestReads)).toBeNull();
  });
});

/* ========================================================================== *
 * feed.hiddenSources —— 隐藏源集合的偏好序列化（高级筛选的持久化底座）
 * ========================================================================== */

describe('feed.hiddenSources 的序列化与读写', () => {
  it('默认值为空串 → 解析出空集合', () => {
    expect(DEFAULT_PREFERENCES['feed.hiddenSources']).toBe('');
    expect(prefIdSet(DEFAULT_PREFERENCES, 'feed.hiddenSources').size).toBe(0);
  });

  it('序列化为升序逗号串，空集合序列化为空串', () => {
    expect(serializeIdSet(new Set([7, 1, 12]))).toBe('1,7,12');
    expect(serializeIdSet(new Set<number>())).toBe('');
  });

  it('解析：正整数入集合，脏数据（非整数/负数/0/空段）被丢弃', () => {
    const prefs = { 'feed.hiddenSources': '1, ,x,-3,0,7,7.5,12' };
    expect([...prefIdSet(prefs, 'feed.hiddenSources')].sort((a, b) => a - b)).toEqual([1, 7, 12]);
  });

  it('setPreference 写入后落 localStorage，且能往返读回', () => {
    useGuestStore.getState().setPreference('feed.hiddenSources', serializeIdSet(new Set([2, 5])));

    const raw = window.localStorage.getItem(StorageKey.guestPreferences);
    expect(raw).not.toBeNull();
    expect(JSON.parse(raw as string)['feed.hiddenSources']).toBe('2,5');
    // 从内存读回同样一致。
    expect([...prefIdSet(useGuestStore.getState().preferences, 'feed.hiddenSources')].sort((a, b) => a - b)).toEqual([
      2, 5,
    ]);
  });

  it('云端偏好合并后，隐藏源集合随 preferences 一起生效（登录跨设备同步路径）', () => {
    useGuestStore.getState().applyCloudState([], [], { 'feed.hiddenSources': '3,9' });

    expect([...prefIdSet(useGuestStore.getState().preferences, 'feed.hiddenSources')].sort((a, b) => a - b)).toEqual([
      3, 9,
    ]);
  });
});