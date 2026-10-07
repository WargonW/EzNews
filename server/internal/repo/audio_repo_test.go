package repo

import (
	"errors"
	"strconv"
	"testing"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// mustTask 造一条合成任务并返回其 ID。
//
// audio_task 有 ux_task_cache(article_id, voice, speed) 唯一约束，
// 同一缓存键只能有一条任务——这是「防重复合成」的锚。
func (f *fixture) mustTask(articleID int64, voice string, speed float64, st model.AudioTaskStatus) int64 {
	f.t.Helper()
	now := util.NowMs()
	var id int64
	f.inTx(func(tx store.Session) error {
		var err error
		id, err = f.audio.InsertTask(f.ctx, tx, &model.AudioTask{
			ArticleID: articleID, Voice: voice, Speed: speed, Status: st,
			TextChars: 100, CreatedAt: now, UpdatedAt: now,
		})
		return err
	})
	return id
}

// mustAudioWithTask 造一条已落盘的音频，同时返回 (audio 主键, 任务主键)。
//
// 需要复用任务主键的用例（重新合成走ON CONFLICT 路径）用这个；
// 只关心 audio 行的用例用 mustAudio。
func (f *fixture) mustAudioWithTask(articleID int64, voice string, sizeBytes, lastAccessAt int64) (int64, int64) {
	f.t.Helper()
	now := util.NowMs()
	taskID := f.mustTask(articleID, voice, 1.0, model.TaskStatusProcessing)
	var id int64
	f.inTx(func(tx store.Session) error {
		var err error
		id, err = f.audio.MarkReady(f.ctx, tx, taskID, &model.Audio{
			TaskID: taskID, ArticleID: articleID, Voice: voice, Speed: 1.0,
			Format: "mp3", FilePath: "a/" + voice + "-" + strconv.FormatInt(articleID, 10) + ".mp3",
			SizeBytes: sizeBytes, DurationMs: 1234, SampleRate: 16000, Provider: "mock",
			LastAccessAt: lastAccessAt, CreatedAt: now,
		}, now)
		return err
	})
	return id, taskID
}

// mustAudio 造一条已落盘的音频记录，返回 audio 主键。
//
// 走 MarkReady 而不是裸 INSERT，为的是同时验证「写 audio + 回填任务」这条复合写。
// taskID 必须指向真实任务行：audio.task_id 有外键指向 audio_task(id)，
// 传 0 会被外键直接拒绝。
func (f *fixture) mustAudio(articleID int64, voice string, sizeBytes, lastAccessAt int64) int64 {
	f.t.Helper()
	id, _ := f.mustAudioWithTask(articleID, voice, sizeBytes, lastAccessAt)
	return id
}

// TestAudioTaskInsertFind_往返_按缓存键命中
//
// 缓存键是三元组 (article_id, voice, speed)：换音色/语速就是另一条任务。
// 任何一维被漏掉当查询条件，都会导致「换了音色还命中旧音频」。
func TestAudioTaskInsertFind_往返_按缓存键命中(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	id := f.mustTask(art, "zh-CN-female", 1.0, model.TaskStatusPending)

	task, err := f.audio.FindTask(f.ctx, art, "zh-CN-female", 1.0)
	if err != nil {
		t.Fatalf("FindTask 失败: %v", err)
	}
	if task.ID != id {
		t.Fatalf("FindTask 返回了不同的行: %d != %d", task.ID, id)
	}
	if task.Status != model.TaskStatusPending {
		t.Fatalf("status 错位: %q", task.Status)
	}
	// 可空列应被IFNULL 兜成零值而不是报错
	if task.Provider != "" || task.AudioID != 0 || task.ErrorCode != "" ||
		task.ErrorMsg != "" || task.NextRetryAt != 0 {
		t.Fatalf("可空列兜底失败: %+v", task)
	}
	if task.RetryCount != 0 || task.TextChars != 100 {
		t.Fatalf("计数列错位: retry=%d chars=%d", task.RetryCount, task.TextChars)
	}
	byID, err := f.audio.GetTask(f.ctx, id)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if byID.Voice != task.Voice || byID.Speed != task.Speed {
		t.Fatalf("两种查法不一致: %+v vs %+v", byID, task)
	}
}

// TestAudioTaskFind_缓存键任一维不同_必须查不到
//
// 三维各自都必须参与匹配。这条把「漏掉一维」这类错误钉死：
// 若 SQL 只按 article_id 查，换了语速也会命中旧任务。
func TestAudioTaskFind_缓存键任一维不同_必须查不到(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	otherArt := f.article(2, articleOpts{})
	f.mustTask(art, "voice-a", 1.0, model.TaskStatusPending)

	cases := []struct {
		name      string
		articleID int64
		voice     string
		speed     float64
	}{
		{"换文章", otherArt, "voice-a", 1.0},
		{"换音色", art, "voice-b", 1.0},
		{"换语速", art, "voice-a", 2.0},
	}
	for _, c := range cases {
		if _, err := f.audio.FindTask(f.ctx, c.articleID, c.voice, c.speed); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s：缓存键不同应查不到，实际 %v", c.name, err)
		}
	}
}

// TestAudioTaskInsert_缓存键重复_必须报错
//
// ux_task_cache 是幂等防翻倍的锚。若这条约束失效，同一篇文章会被重复合成，
// 白烧 TTS 配额。
func TestAudioTaskInsert_缓存键重复_必须报错(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	f.mustTask(art, "voice-a", 1.0, model.TaskStatusPending)

	now := util.NowMs()
	f.inTx(func(tx store.Session) error {
		_, err := f.audio.InsertTask(f.ctx, tx, &model.AudioTask{
			ArticleID: art, Voice: "voice-a", Speed: 1.0,
			Status: model.TaskStatusPending, TextChars: 100,
			CreatedAt: now, UpdatedAt: now,
		})
		if err == nil {
			t.Fatal("重复的缓存键必须被唯一索引拒绝")
		}
		return nil
	})
	if n := f.scalarI64("SELECT COUNT(*) FROM audio_task WHERE article_id=?", art); n != 1 {
		t.Fatalf("重复插入不应留下第二行，实际 %d 行", n)
	}
}

// TestAudioTaskClaim_只能从pending抢到一次
//
// ★ ClaimTask 是 CAS：SQL 里带 AND status='pending'，靠 RowsAffected 判成败。
// 这是「同一个任务不被两个 worker 同时合成」的唯一保证——
// 若去掉那个条件，两个 worker 会同时抢到同一任务，TTS 调用翻倍。
func TestAudioTaskClaim_只能从pending抢到一次(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	id := f.mustTask(art, "voice-a", 1.0, model.TaskStatusPending)

	var ok bool
	f.inTx(func(tx store.Session) error {
		var err error
		ok, err = f.audio.ClaimTask(f.ctx, tx, id, 1_700_000_100)
		return err
	})
	if !ok {
		t.Fatal("pending 任务首次抢占应成功")
	}
	task, err := f.audio.GetTask(f.ctx, id)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if task.Status != model.TaskStatusProcessing {
		t.Fatalf("抢占后状态应为 processing，实际 %q", task.Status)
	}
	if task.UpdatedAt != 1_700_000_100 {
		t.Fatalf("updated_at 未推进: %d", task.UpdatedAt)
	}

	// 第二次必须失败：此时状态已是 processing
	f.inTx(func(tx store.Session) error {
		var err error
		ok, err = f.audio.ClaimTask(f.ctx, tx, id, 1_700_000_200)
		return err
	})
	if ok {
		t.Fatal("已被抢占的任务不得被二次抢占")
	}
	// 状态不得被第二次抢占改动
	task, err = f.audio.GetTask(f.ctx, id)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if task.UpdatedAt != 1_700_000_100 {
		t.Fatalf("失败的抢占不应改动 updated_at，实际 %d", task.UpdatedAt)
	}
}

// TestAudioTaskClaim_非pending状态一律抢不到
func TestAudioTaskClaim_非pending状态一律抢不到(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	for i, st := range []model.AudioTaskStatus{
		model.TaskStatusReady, model.TaskStatusFailed, model.TaskStatusProcessing,
	} {
		id := f.mustTask(art, "voice-"+string(rune('a'+i)), 1.0, st)
		var ok bool
		f.inTx(func(tx store.Session) error {
			var err error
			ok, err = f.audio.ClaimTask(f.ctx, tx, id, 1_700_000_100)
			return err
		})
		if ok {
			t.Fatalf("状态 %q 的任务不应被抢占", st)
		}
	}
}

// TestAudioMarkReady_首次写入_回填audio_id并把任务转为ready
//
// MarkReady 是一个事务里的两笔复合写（写 audio + 更新 audio_task）。
// 任一笔失败都必须整体回滚，否则会出现「任务 ready 但 audio 行不存在」，
// 前端拿到一个指向空行的 audioId。
func TestAudioMarkReady_首次写入_回填音频ID并把任务转为ready(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	taskID := f.mustTask(art, "voice-a", 1.0, model.TaskStatusProcessing)
	now := util.NowMs()

	var audioID int64
	f.inTx(func(tx store.Session) error {
		var err error
		audioID, err = f.audio.MarkReady(f.ctx, tx, taskID, &model.Audio{
			TaskID: taskID, ArticleID: art, Voice: "voice-a", Speed: 1.0,
			Format: "mp3", FilePath: "a/1.mp3", SizeBytes: 999,
			DurationMs: 4321, SampleRate: 16000, Provider: "mock",
			LastAccessAt: now, CreatedAt: now,
		}, now)
		return err
	})
	if audioID == 0 {
		t.Fatal("MarkReady 必须返回 audio主键")
	}
	a, err := f.audio.GetAudio(f.ctx, audioID)
	if err != nil {
		t.Fatalf("GetAudio 失败: %v", err)
	}
	if a.ArticleID != art || a.FilePath != "a/1.mp3" || a.SizeBytes != 999 || a.DurationMs != 4321 {
		t.Fatalf("audio 行错位: %+v", a)
	}
	if a.TaskID != taskID || a.Provider != "mock" {
		t.Fatalf("task_id/provider 未回填: %+v", a)
	}
	task, err := f.audio.GetTask(f.ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if task.Status != model.TaskStatusReady {
		t.Fatalf("任务状态应为 ready，实际 %q", task.Status)
	}
	if task.AudioID != audioID {
		t.Fatalf("audio_id 未回填到任务，实际 %d 期望 %d", task.AudioID, audioID)
	}
}

// TestAudioMarkReady_重复调用_必须走冲突更新且返回同一个audioID
//
// ★ ON CONFLICT(article_id,voice,speed) DO UPDATE：重新合成走这条路。
// 关键不变量是「返回的 audioID 与库里的行一致」——实现里为此专门做了回查，
// 因为冲突更新路径下 LastInsertId 不可靠。若这个回查被删掉，
// 第二次调用会返回一个错误的 id，任务的 audio_id 就指错了行。
func TestAudioMarkReady_重复调用_必须走冲突更新且返回同一个音频ID(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	now := util.NowMs()
	first, firstTaskID := f.mustAudioWithTask(art, "voice-a", 1000, now)

	var second int64
	f.inTx(func(tx store.Session) error {
		var err error
		second, err = f.audio.MarkReady(f.ctx, tx, 0, &model.Audio{
			TaskID: firstTaskID, ArticleID: art, Voice: "voice-a", Speed: 1.0, Format: "mp3",
			FilePath: "a/1.mp3", SizeBytes: 2000,
			DurationMs: 5000, SampleRate: 24000, Provider: "ali",
			LastAccessAt: now + 100, CreatedAt: now,
		}, now)
		return err
	})
	if second != first {
		t.Fatalf("冲突更新后audioID 应不变，实际 %d -> %d", first, second)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM audio WHERE article_id=?", art); n != 1 {
		t.Fatalf("冲突更新不得新增行，实际 %d 行", n)
	}
	// 派生字段应被更新为新值
	a, err := f.audio.GetAudio(f.ctx, first)
	if err != nil {
		t.Fatalf("GetAudio 失败: %v", err)
	}
	if a.SizeBytes != 2000 || a.DurationMs != 5000 || a.SampleRate != 24000 || a.Provider != "ali" {
		t.Fatalf("冲突更新未刷新派生字段: %+v", a)
	}
	if a.LastAccessAt != now+100 {
		t.Fatalf("last_access_at 未刷新: %d", a.LastAccessAt)
	}
	// hit_count 不在 SET 列表里，必须保留原值
	if a.HitCount != 0 {
		t.Fatalf("hit_count 不应被冲突更新重置，实际 %d", a.HitCount)
	}
}

// TestAudioMarkReady_新路径为空_必须保留原文件路径
//
// ★ 这是 ON CONFLICT 里最微妙的一处：重新合成时，文件还没落盘，
// file_path 会先传空串进来。SQL 用 CASE WHEN excluded.file_path <> ” 判掉，
// 保住旧路径；等文件真的写完再由后续 UPDATE 修正。
// 若这个 CASE 退化成直接赋值，音频记录会短暂指向空路径，播放直接 404。
func TestAudioMarkReady_新路径为空_必须保留原文件路径(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	now := util.NowMs()
	id, taskID := f.mustAudioWithTask(art, "voice-a", 1000, now)
	before, err := f.audio.GetAudio(f.ctx, id)
	if err != nil {
		t.Fatalf("GetAudio 失败: %v", err)
	}

	f.inTx(func(tx store.Session) error {
		_, err := f.audio.MarkReady(f.ctx, tx, taskID, &model.Audio{
			TaskID: taskID, ArticleID: art, Voice: "voice-a", Speed: 1.0, Format: "mp3",
			FilePath: "", SizeBytes: 2000,
			DurationMs: 5000, SampleRate: 24000, Provider: "ali",
			LastAccessAt: now, CreatedAt: now,
		}, now)
		return err
	})
	a, err := f.audio.GetAudio(f.ctx, id)
	if err != nil {
		t.Fatalf("GetAudio 失败: %v", err)
	}
	if a.FilePath != before.FilePath {
		t.Fatalf("空路径不得覆盖已有路径，期望 %q 实际 %q", before.FilePath, a.FilePath)
	}
	// 其余字段仍应被刷新
	if a.SizeBytes != 2000 {
		t.Fatalf("size_bytes 未刷新: %d", a.SizeBytes)
	}
}

// TestAudioMarkRetry_回退为pending并记录退避时间与错误
func TestAudioMarkRetry_回退为pending并记录退避时间与错误(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	id := f.mustTask(art, "voice-a", 1.0, model.TaskStatusProcessing)

	f.inTx(func(tx store.Session) error {
		return f.audio.MarkRetry(f.ctx, tx, id, 2, 1_700_000_500, 1_700_000_400, "E_RATE", "限流")
	})
	task, err := f.audio.GetTask(f.ctx, id)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if task.Status != model.TaskStatusPending {
		t.Fatalf("状态应回退为 pending，实际 %q", task.Status)
	}
	if task.RetryCount != 2 || task.NextRetryAt != 1_700_000_500 {
		t.Fatalf("重试计数/退避时间不对: retry=%d next=%d", task.RetryCount, task.NextRetryAt)
	}
	if task.ErrorCode != "E_RATE" || task.ErrorMsg != "限流" {
		t.Fatalf("错误信息未记录: code=%q msg=%q", task.ErrorCode, task.ErrorMsg)
	}
}

// TestAudioMarkFailed_置为failed并清空退避时间
//
// next_retry_at 必须被清成 NULL：留着它会让失败任务被 ListRetryDue
// 反复捞起来重投，永远停不下来。
func TestAudioMarkFailed_置为failed并清空退避时间(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	id := f.mustTask(art, "voice-a", 1.0, model.TaskStatusProcessing)
	f.inTx(func(tx store.Session) error {
		return f.audio.MarkRetry(f.ctx, tx, id, 5, 1_700_000_500, 1_700_000_400, "E_X", "临时失败")
	})

	f.inTx(func(tx store.Session) error {
		return f.audio.MarkFailed(f.ctx, tx, id, "E_FATAL", "配额耗尽", 1_700_000_600)
	})
	task, err := f.audio.GetTask(f.ctx, id)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if task.Status != model.TaskStatusFailed {
		t.Fatalf("状态应为 failed，实际 %q", task.Status)
	}
	if task.ErrorCode != "E_FATAL" || task.ErrorMsg != "配额耗尽" {
		t.Fatalf("错误信息未更新: %+v", task)
	}
	if task.NextRetryAt != 0 {
		t.Fatalf("next_retry_at 必须被清空，实际 %d", task.NextRetryAt)
	}
	// retry_count 保留（用于观测重试了几次）
	if task.RetryCount != 5 {
		t.Fatalf("retry_count 应保留，实际 %d", task.RetryCount)
	}
}

// TestAudioRequeue_只有失败任务可被重投
//
// SQL 带 AND status='failed'：重投接口是幂等的重复点击保护，
// 在 processing/ready 上调用必须是空操作。若去掉这个条件，
// 重复点击「重试」会把正在合成的任务打回pending，造成重复合成。
func TestAudioRequeue_只有失败任务可被重投(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	failed := f.mustTask(art, "voice-f", 1.0, model.TaskStatusFailed)
	proc := f.mustTask(art, "voice-p", 1.0, model.TaskStatusProcessing)
	ready := f.mustTask(art, "voice-r", 1.0, model.TaskStatusReady)
	pending := f.mustTask(art, "voice-n", 1.0, model.TaskStatusPending)

	// 先给失败任务造上脏数据，验证重投会清干净
	f.inTx(func(tx store.Session) error {
		return f.audio.MarkRetry(f.ctx, tx, failed, 9, 1_700_000_500, 1_700_000_400, "E_X", "旧错")
	})
	f.inTx(func(tx store.Session) error {
		return f.audio.MarkFailed(f.ctx, tx, failed, "E_FATAL", "配额耗尽", 1_700_000_600)
	})

	if err := f.audio.RequeueTask(f.ctx, failed, 1_700_000_700); err != nil {
		t.Fatalf("重投失败任务失败: %v", err)
	}
	task, err := f.audio.GetTask(f.ctx, failed)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if task.Status != model.TaskStatusPending {
		t.Fatalf("重投后状态应为 pending，实际 %q", task.Status)
	}
	if task.RetryCount != 0 {
		t.Fatalf("重投应清空 retry_count，实际 %d", task.RetryCount)
	}
	if task.NextRetryAt != 0 || task.ErrorCode != "" || task.ErrorMsg != "" {
		t.Fatalf("重投应清空退避与错误信息: %+v", task)
	}

	// 其余状态不得被影响
	for _, id := range []int64{proc, ready, pending} {
		if err := f.audio.RequeueTask(f.ctx, id, 1_700_000_700); err != nil {
			t.Fatalf("非failed 任务重投应为空操作且不报错: %v", err)
		}
		got, err := f.audio.GetTask(f.ctx, id)
		if err != nil {
			t.Fatalf("GetTask 失败: %v", err)
		}
		if got.Status == model.TaskStatusPending && got.ID != pending {
			t.Fatalf("任务 %d 被误重投成 pending", id)
		}
	}
}

// TestAudioListRetryDue_两类任务都要捞且按退避时间排序
//
// ① next_retry_at <= now的退避到期；
// ② next_retry_at IS NULL 且 updated_at <= now-5s 的「从未入队」任务
//
//	（创建时队列满被漏掉的兜底）。
//
// 漏掉② 会让那些任务永久卡在 pending 无人处理。
func TestAudioListRetryDue_两类任务都要捞且按退避时间排序(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	arts := []int64{
		f.article(1, articleOpts{}), f.article(2, articleOpts{}),
		f.article(3, articleOpts{}), f.article(4, articleOpts{}),
	}
	// 退避未到
	notYet := f.mustTask(arts[0], "v0", 1.0, model.TaskStatusPending)
	// 退避已到（早）
	dueEarly := f.mustTask(arts[1], "v1", 1.0, model.TaskStatusPending)
	// 从未入队、且已静默超过 5s
	silent := f.mustTask(arts[2], "v2", 1.0, model.TaskStatusPending)
	// 从未入队、但刚刚创建（还在5s 静默期内）
	freshTask := f.mustTask(arts[3], "v3", 1.0, model.TaskStatusPending)

	now = util.NowMs()
	f.inTx(func(tx store.Session) error {
		if err := f.audio.MarkRetry(f.ctx, tx, notYet, 1, now+60_000, now, "", ""); err != nil {
			return err
		}
		if err := f.audio.MarkRetry(f.ctx, tx, dueEarly, 1, now-60_000, now, "", ""); err != nil {
			return err
		}
		return f.audio.MarkRetry(f.ctx, tx, silent, 0, 0, now-10_000, "", "")
	})

	ids, err := f.audio.ListRetryDue(f.ctx, now, 100)
	if err != nil {
		t.Fatalf("ListRetryDue 失败: %v", err)
	}
	got := map[int64]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[dueEarly] || !got[silent] {
		t.Fatalf("退避到期与静默超时的任务都应被捞，实际 %v", ids)
	}
	if got[notYet] {
		t.Fatal("退避未到的任务不该被捞")
	}
	if got[freshTask] {
		t.Fatal("仍在静默期内的任务不该被捞")
	}
	// 排序：ORDER BY next_retry_at, id。SQLite 里 NULL 排最前，
	// 所以「从未入队」（next_retry_at IS NULL）的任务优先于「退避到期」——
	// 这个次序是合理的：没试过的活儿比已经退避过的更该先跑。
	// 这条断言的作用是把NULL-first 这个事实钉住，防止有人给SQL 加
	// `ORDER BY next_retry_at IS NULL, next_retry_at` 把次序反过来时无人察觉。
	if len(ids) != 2 || ids[0] != silent || ids[1] != dueEarly {
		t.Fatalf("排序应为 [静默超时, 退避到期]，实际 %v", ids)
	}
	// limit 必须生效
	one, err := f.audio.ListRetryDue(f.ctx, now, 1)
	if err != nil {
		t.Fatalf("ListRetryDue 失败: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际 %d", len(one))
	}
}

// TestAudioListRetryDue_只捞pending_其他状态一律排除
func TestAudioListRetryDue_只捞pending_其他状态一律排除(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	art := f.article(1, articleOpts{})
	var ids []int64
	for i, st := range []model.AudioTaskStatus{
		model.TaskStatusProcessing, model.TaskStatusReady, model.TaskStatusFailed,
		model.TaskStatusPending,
	} {
		ids = append(ids, f.mustTask(art, "voice-"+string(rune('a'+i)), 1.0, st))
	}
	now = util.NowMs()
	f.inTx(func(tx store.Session) error {
		return f.audio.MarkRetry(f.ctx, tx, ids[3], 1, now-1000, now-20000, "", "")
	})
	due, err := f.audio.ListRetryDue(f.ctx, now, 100)
	if err != nil {
		t.Fatalf("ListRetryDue 失败: %v", err)
	}
	if len(due) != 1 || due[0] != ids[3] {
		t.Fatalf("只应捞到 pending 那一条，实际 %v", due)
	}
}

// TestAudioListPending_按id升序且只含pending
func TestAudioListPending_按id升序且只含pending(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	p1 := f.mustTask(art, "va", 1.0, model.TaskStatusPending)
	f.mustTask(art, "vb", 1.0, model.TaskStatusProcessing)
	p2 := f.mustTask(art, "vc", 1.0, model.TaskStatusPending)
	p3 := f.mustTask(art, "vd", 1.0, model.TaskStatusPending)

	ids, err := f.audio.ListPending(f.ctx, 100)
	if err != nil {
		t.Fatalf("ListPending 失败: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(ids))
	}
	// 队列恢复后必须按入队顺序（FIFO）处理，所以要严格 id ASC
	for i, want := range []int64{p1, p2, p3} {
		if ids[i] != want {
			t.Fatalf("顺序应按 id 升序，位置 %d 期望 %d 实际 %d", i, want, ids[i])
		}
	}
	// limit
	lim, err := f.audio.ListPending(f.ctx, 2)
	if err != nil {
		t.Fatalf("ListPending 失败: %v", err)
	}
	if len(lim) != 2 || lim[0] != p1 {
		t.Fatalf("limit=2 应返回前两条，实际 %v", lim)
	}
	// 空队列单独开一个 fixture——本fixture 里已经造了 3 条 pending
	t.Run("空队列", func(t *testing.T) {
		f2 := newFixture(t)
		empty, err := f2.audio.ListPending(f2.ctx, 10)
		if err != nil {
			t.Fatalf("ListPending 失败: %v", err)
		}
		if len(empty) != 0 {
			t.Fatalf("空队列应返回空，实际 %v", empty)
		}
	})
}

// TestAudioResetStaleProcessing_只重置超时的processing
//
// 进程崩溃会留下卡在 processing 的任务，重启后必须把它们捞回 pending。
// 时间条件是 updated_at < before——不能把刚被抢占的任务误重置，
// 否则同一个任务会被当前 worker 和新 worker 同时处理。
func TestAudioResetStaleProcessing_只重置超时的processing(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	now := util.NowMs()
	stale := f.mustTask(art, "va", 1.0, model.TaskStatusProcessing)
	fresh := f.mustTask(art, "vb", 1.0, model.TaskStatusProcessing)
	pending := f.mustTask(art, "vc", 1.0, model.TaskStatusPending)

	// 手工调时间：stale 很久以前、fresh 刚刚
	if _, err := f.db.ExecContext(f.ctx,
		"UPDATE audio_task SET updated_at=? WHERE id=?", now-60_000, stale); err != nil {
		t.Fatalf("调整时间失败: %v", err)
	}
	// 给 stale 造一个 next_retry_at，验证重置时会被清空
	f.inTx(func(tx store.Session) error {
		return f.audio.MarkRetry(f.ctx, tx, stale, 1, now+1000, now-60_000, "", "")
	})
	f.inTx(func(tx store.Session) error {
		var err error
		_, err = f.audio.ClaimTask(f.ctx, tx, stale, now-60_000)
		return err
	})

	n, err := f.audio.ResetStaleProcessing(f.ctx, now-30_000, now)
	if err != nil {
		t.Fatalf("ResetStaleProcessing 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("只应重置 1 条，实际 %d", n)
	}
	got, err := f.audio.GetTask(f.ctx, stale)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("超时任务应被重置为 pending，实际 %q", got.Status)
	}
	if got.NextRetryAt != 0 {
		t.Fatalf("重置应清空 next_retry_at，实际 %d", got.NextRetryAt)
	}
	if got.UpdatedAt != now {
		t.Fatalf("重置应刷新 updated_at 为 now，实际 %d", got.UpdatedAt)
	}
	// 未超时的 processing 任务不得被重置（否则当前 worker 与新 worker 会重复处理）
	freshGot, err := f.audio.GetTask(f.ctx, fresh)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if freshGot.Status != model.TaskStatusProcessing {
		t.Fatalf("未超时的 processing 被误重置为 %q", freshGot.Status)
	}
	// 本就 pending 的任务不得被计入重置条数
	pendingGot, err := f.audio.GetTask(f.ctx, pending)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if pendingGot.Status != model.TaskStatusPending {
		t.Fatalf("本就pending 的任务状态被改成了 %q", pendingGot.Status)
	}
}

// TestAudioTotalAudioBytes_空表必须返回0而不是报错
//
// SQL 用 SUM(size_bytes)，空表时聚合结果是一行 NULL。
// 扫描进 sql.NullInt64 才能兜成 0——若直接 Scan 进 int64 会得到 ErrNullValue，
// LRU 清理的入口直接崩。
func TestAudioTotalAudioBytes_空表必须返回0而不是报错(t *testing.T) {
	f := newFixture(t)
	total, err := f.audio.TotalAudioBytes(f.ctx)
	if err != nil {
		t.Fatalf("空表统计不应报错: %v", err)
	}
	if total != 0 {
		t.Fatalf("空表应返回 0，实际 %d", total)
	}

	art1 := f.article(1, articleOpts{})
	art2 := f.article(2, articleOpts{})
	now := util.NowMs()
	f.mustAudio(art1, "va", 1000, now)
	f.mustAudio(art2, "vb", 2500, now)
	total, err = f.audio.TotalAudioBytes(f.ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != 3500 {
		t.Fatalf("总数应为 3500，实际 %d", total)
	}
}

// TestAudioListForCleanup_按LRU升序且以id兜底
//
// ★ 清理顺序就是删除顺序：先删最久没访问的。
// last_access_at 不唯一（同一毫秒访问的两条），所以 id ASC 是必需的 tiebreaker——
// 否则两条访问时间相同的音频，清理顺序会随SQLite 的物理顺序抖动，
// 配额一紧张就会随机踢掉「刚好刚被访问过」的那条。
func TestAudioListForCleanup_按LRU升序且以id兜底(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	var artIDs []int64
	for i := 0; i < 5; i++ {
		artIDs = append(artIDs, f.article(i+1, articleOpts{}))
	}
	// 5 条音频，last_access_at 全部相同（考察 id tiebreaker）
	var ids []int64
	for i, art := range artIDs {
		ids = append(ids, f.mustAudio(art, "v"+string(rune('a'+i)), 100, now))
	}
	// 再造一条 LRU 更早的
	oldest := f.mustAudio(f.article(99, articleOpts{}), "vz", 100, now-100_000)

	list, err := f.audio.ListAudioForCleanup(f.ctx, now+1, 100)
	if err != nil {
		t.Fatalf("ListAudioForCleanup 失败: %v", err)
	}
	if len(list) != 6 {
		t.Fatalf("应返回 6 条，实际 %d", len(list))
	}
	if list[0].ID != oldest {
		t.Fatalf("最久未访问的应排第一，实际 %d", list[0].ID)
	}
	for i := 1; i < 6; i++ {
		if list[i].ID != ids[i-1] {
			t.Fatalf("同时刻条目必须按 id 升序，位置 %d 期望 %d 实际 %d",
				i, ids[i-1], list[i].ID)
		}
		if list[i-1].LastAccessAt > list[i].LastAccessAt {
			t.Fatalf("last_access_at 必须升序: %d > %d",
				list[i-1].LastAccessAt, list[i].LastAccessAt)
		}
	}
	// 边界：last_access_at 等于阈值时不应被选中（严格小于）
	edge, err := f.audio.ListAudioForCleanup(f.ctx, now, 100)
	if err != nil {
		t.Fatalf("ListAudioForCleanup 失败: %v", err)
	}
	if len(edge) != 1 || edge[0].ID != oldest {
		t.Fatalf("last_access_at == 阈值的不该被选中，实际 %d 条", len(edge))
	}
	// limit
	lim, err := f.audio.ListAudioForCleanup(f.ctx, now+1, 2)
	if err != nil {
		t.Fatalf("ListAudioForCleanup 失败: %v", err)
	}
	if len(lim) != 2 {
		t.Fatalf("limit=2 应返回 2 条，实际 %d", len(lim))
	}
}

// TestAudioBatchTaskStatus_空入参返回空map
//
// 空列表必须短路返回，不能拼出 "IN ()" 这种语法错误的 SQL。
func TestAudioBatchTaskStatus_空入参返回空map(t *testing.T) {
	f := newFixture(t)
	m, err := f.audio.BatchTaskStatus(f.ctx, nil, "voice-a", 1.0)
	if err != nil {
		t.Fatalf("空入参不应报错: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("空入参应返回空 map，实际 %d 条", len(m))
	}
	m, err = f.audio.BatchTaskStatus(f.ctx, []int64{}, "voice-a", 1.0)
	if err != nil {
		t.Fatalf("空切片不应报错: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("空切片应返回空 map，实际 %d 条", len(m))
	}
}

// TestAudioBatchTaskStatus_按音色语速过滤且未命中的文章不进map
//
// 这段 SQL 是给 withAudio 用的批量查询，替代 N+1。
// 语义是「命中即返回，不命中就是不出现」——上层把缺失的键当作 none。
// 若未命中的文章被塞进一个零值 Brief，前端会显示成「合成中」。
func TestAudioBatchTaskStatus_按音色语速过滤且未命中的文章不进map(t *testing.T) {
	f := newFixture(t)
	art1 := f.article(1, articleOpts{})
	art2 := f.article(2, articleOpts{})
	art3 := f.article(3, articleOpts{})
	f.mustTask(art1, "voice-a", 1.0, model.TaskStatusProcessing)
	f.mustTask(art2, "voice-a", 1.0, model.TaskStatusFailed)
	readyID := f.mustAudio(art3, "voice-a", 100, util.NowMs())
	// 别的音色/语速，不该被命中
	f.mustTask(art3, "voice-b", 1.0, model.TaskStatusPending)
	f.mustTask(art3, "voice-a", 2.0, model.TaskStatusPending)

	m, err := f.audio.BatchTaskStatus(f.ctx, []int64{art1, art2, art3, 999999}, "voice-a", 1.0)
	if err != nil {
		t.Fatalf("BatchTaskStatus 失败: %v", err)
	}
	if len(m) != 3 {
		t.Fatalf("只应返回 3 条命中记录，实际 %d", len(m))
	}
	if _, ok := m[999999]; ok {
		t.Fatal("没有任务的文章不应出现在结果里")
	}
	if m[art1].Status != string(model.TaskStatusProcessing) {
		t.Fatalf("art1 状态错位: %q", m[art1].Status)
	}
	// 未 ready 的 audio_id 应为 nil（而不是 0）
	if m[art1].AudioID != nil {
		t.Fatalf("processing 任务的 audioId 应为 nil，实际 %d", *m[art1].AudioID)
	}
	if m[art2].Status != string(model.TaskStatusFailed) {
		t.Fatalf("art2 状态错位: %q", m[art2].Status)
	}
	// ready 的应回填 audio_id
	if m[art3].AudioID == nil || *m[art3].AudioID != readyID {
		t.Fatalf("ready 任务应回填 audioId=%d，实际 %v", readyID, m[art3].AudioID)
	}
	// voice/speed 应回填为入参
	if m[art1].Voice == nil || *m[art1].Voice != "voice-a" {
		t.Fatalf("voice 未回填: %v", m[art1].Voice)
	}
	if m[art1].Speed == nil || *m[art1].Speed != 1.0 {
		t.Fatalf("speed 未回填: %v", m[art1].Speed)
	}
}

// TestAudioBatchTaskStatus_每条目的指针必须互相独立
//
// 实现里有个刻意的修正：逐条拷贝 voice/speed 再取地址。
// 若图省事写 &voice / &speed，map 里所有条目会共享同一地址——
// 现在只读无害，将来有人原地改就会把所有条目一起改掉。
func TestAudioBatchTaskStatus_每条目的指针必须互相独立(t *testing.T) {
	f := newFixture(t)
	art1 := f.article(1, articleOpts{})
	art2 := f.article(2, articleOpts{})
	f.mustTask(art1, "voice-a", 1.0, model.TaskStatusPending)
	f.mustTask(art2, "voice-a", 1.0, model.TaskStatusPending)

	m, err := f.audio.BatchTaskStatus(f.ctx, []int64{art1, art2}, "voice-a", 1.0)
	if err != nil {
		t.Fatalf("BatchTaskStatus 失败: %v", err)
	}
	a, b := m[art1], m[art2]
	if a.Voice == b.Voice {
		t.Fatal("两条记录的 voice 指针不应共享同一地址")
	}
	if a.Speed == b.Speed {
		t.Fatal("两条记录的 speed 指针不应共享同一地址")
	}
}

// TestAudioFindByArticleIDs_分片查询不重不漏
//
// chunkInt64 把长列表按 500 分片。这里造 1201 条（跨 3 片）来验证分片拼接。
func TestAudioFindByArticleIDs_分片查询不重不漏(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	const n = 1201
	var artIDs, expectAudio []int64
	for i := 0; i < n; i++ {
		art := f.article(i+1, articleOpts{})
		artIDs = append(artIDs, art)
		// 每 3 篇造一条音频
		if i%3 == 0 {
			expectAudio = append(expectAudio, f.mustAudio(art, "v", 10, now))
		}
	}
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	list, err := f.audio.FindAudioByArticleIDs(f.ctx, tx, artIDs)
	if err != nil {
		t.Fatalf("FindAudioByArticleIDs 失败: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if len(list) != len(expectAudio) {
		t.Fatalf("跨分片查询应返回 %d 条，实际 %d", len(expectAudio), len(list))
	}
	seen := map[int64]int{}
	for _, a := range list {
		seen[a.ID]++
	}
	for _, id := range expectAudio {
		if seen[id] != 1 {
			t.Fatalf("音频 %d 应恰好出现 1 次，实际 %d 次", id, seen[id])
		}
	}
	// 空入参
	f.inTx(func(tx store.Session) error {
		got, err := f.audio.FindAudioByArticleIDs(f.ctx, tx, nil)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			t.Fatalf("空入参应返回空，实际 %d 条", len(got))
		}
		return nil
	})
}

// TestAudioDeleteIDs_删除后行消失且不影响其他行
func TestAudioDeleteIDs_删除后行消失且不影响其他行(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	a1 := f.mustAudio(f.article(1, articleOpts{}), "v1", 10, now)
	a2 := f.mustAudio(f.article(2, articleOpts{}), "v2", 20, now)
	a3 := f.mustAudio(f.article(3, articleOpts{}), "v3", 30, now)

	f.inTx(func(tx store.Session) error {
		return f.audio.DeleteAudioIDs(f.ctx, tx, []int64{a1, a3})
	})
	if f.rows("audio") != 1 {
		t.Fatalf("audio 表应剩 1 行，实际 %d", f.rows("audio"))
	}
	if _, err := f.audio.GetAudio(f.ctx, a1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a1 应已删除，实际 %v", err)
	}
	if _, err := f.audio.GetAudio(f.ctx, a3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a3 应已删除，实际 %v", err)
	}
	if _, err := f.audio.GetAudio(f.ctx, a2); err != nil {
		t.Fatalf("a2 不该被删: %v", err)
	}
	// 删不存在的 id 不应报错
	f.inTx(func(tx store.Session) error {
		return f.audio.DeleteAudioIDs(f.ctx, tx, []int64{999999})
	})
	// 空入参
	f.inTx(func(tx store.Session) error {
		return f.audio.DeleteAudioIDs(f.ctx, tx, nil)
	})
}

// TestAudioTouchAccess_累加而不是覆盖
//
// hit_count 必须用 hit_count + 1。若写成 hit_count=?，每次刷盘都把计数抹成 1，
// LRU 与热门度统计全废。
func TestAudioTouchAccess_累加而不是覆盖(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	a1 := f.mustAudio(f.article(1, articleOpts{}), "v1", 10, now)
	a2 := f.mustAudio(f.article(2, articleOpts{}), "v2", 20, now)

	for i := 0; i < 3; i++ {
		if err := f.audio.TouchAccess(f.ctx, []int64{a1}, now+int64(i+1)); err != nil {
			t.Fatalf("TouchAccess 失败: %v", err)
		}
	}
	if err := f.audio.TouchAccess(f.ctx, []int64{a1, a2}, now+100); err != nil {
		t.Fatalf("TouchAccess 失败: %v", err)
	}
	got, err := f.audio.GetAudio(f.ctx, a1)
	if err != nil {
		t.Fatalf("GetAudio 失败: %v", err)
	}
	if got.HitCount != 4 {
		t.Fatalf("a1 应被访问 4 次，实际 %d", got.HitCount)
	}
	if got.LastAccessAt != now+100 {
		t.Fatalf("last_access_at 应刷新为 %d，实际 %d", now+100, got.LastAccessAt)
	}
	got2, err := f.audio.GetAudio(f.ctx, a2)
	if err != nil {
		t.Fatalf("GetAudio 失败: %v", err)
	}
	if got2.HitCount != 1 || got2.LastAccessAt != now+100 {
		t.Fatalf("a2 应被访问 1 次且时间刷新，实际 %+v", got2)
	}
	// 空入参不得拼出语法错误的 SQL
	if err := f.audio.TouchAccess(f.ctx, nil, now+200); err != nil {
		t.Fatalf("空入参不应报错: %v", err)
	}
	// 不存在的 id 不应报错也不该影响别的行
	if err := f.audio.TouchAccess(f.ctx, []int64{999999}, now+300); err != nil {
		t.Fatalf("不存在的 id 不应报错: %v", err)
	}
	got, err = f.audio.GetAudio(f.ctx, a1)
	if err != nil {
		t.Fatalf("GetAudio 失败: %v", err)
	}
	if got.HitCount != 4 {
		t.Fatalf("无效 id 不应影响其他行，实际 hit_count=%d", got.HitCount)
	}
}

// TestAudioMarkReady_任务与音频必须同生共死
//
// MarkReady 是「写 audio + 更新 audio_task」两笔写。
// 若其中一笔绕过 tx 走连接池，或整体不在一个事务里，
// 就会出现「任务已 ready 但 audio 行不存在」的幽灵状态。
// 这里用「整个事务回滚」验证二者原子绑定。
func TestAudioMarkReady_任务与音频必须同生共死(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	taskID := f.mustTask(art, "voice-a", 1.0, model.TaskStatusProcessing)
	now := util.NowMs()

	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if _, err := f.audio.MarkReady(f.ctx, tx, taskID, &model.Audio{
		TaskID: taskID, ArticleID: art, Voice: "voice-a", Speed: 1.0,
		Format: "mp3", FilePath: "a/1.mp3", SizeBytes: 100,
		DurationMs: 200, SampleRate: 16000, LastAccessAt: now, CreatedAt: now,
	}, now); err != nil {
		t.Fatalf("MarkReady 失败: %v", err)
	}
	// 事务内先确认两笔写都生效
	if n := f.scalarI64Tx(f.ctx, tx, "SELECT COUNT(*) FROM audio WHERE article_id=?", art); n != 1 {
		t.Fatalf("事务内 audio 行应存在，实际 %d", n)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	// 回滚后两笔写都必须消失
	if n := f.scalarI64("SELECT COUNT(*) FROM audio WHERE article_id=?", art); n != 0 {
		t.Fatalf("回滚后 audio 行应消失，实际 %d", n)
	}
	task, err := f.audio.GetTask(f.ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask 失败: %v", err)
	}
	if task.Status != model.TaskStatusProcessing {
		t.Fatalf("回滚后任务状态应仍是 processing，实际 %q", task.Status)
	}
	if task.AudioID != 0 {
		t.Fatalf("回滚后 audio_id 应仍是 0，实际 %d", task.AudioID)
	}
}

// TestAudioGet_不存在_返回ErrNotFound
func TestAudioGet_不存在_返回ErrNotFound(t *testing.T) {
	f := newFixture(t)
	art := f.article(1, articleOpts{})
	f.mustTask(art, "voice-a", 1.0, model.TaskStatusPending)
	if _, err := f.audio.FindAudio(f.ctx, art, "voice-a", 1.0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的音频应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := f.audio.GetAudio(f.ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAudio 不存在应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := f.audio.GetTask(f.ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask 不存在应返回 ErrNotFound，实际 %v", err)
	}
}
