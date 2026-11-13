// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"
)

// chanConvStore 会话簿存储替身：Open 恒按新建返回 fresh；SaveTurns 把批次投进
// saved（channel 屏障——用例按收到批次判定 Flush 已发生，不靠 sleep）。
type chanConvStore struct {
	saved chan []codexsdk.TurnWrite
}

func newChanConvStore() *chanConvStore {
	return &chanConvStore{saved: make(chan []codexsdk.TurnWrite, 16)}
}

func (s *chanConvStore) Open(_ context.Context, _ codexsdk.ConversationKey, fresh codexsdk.ConversationRecord) (codexsdk.ConversationRecord, bool, error) {
	return fresh, true, nil
}

func (s *chanConvStore) Advance(_ context.Context, _ codexsdk.ConversationKey, _, _ uint64, _ string) (codexsdk.ConversationRecord, error) {
	return codexsdk.ConversationRecord{}, nil
}

func (s *chanConvStore) Load(_ context.Context, _ codexsdk.ConversationKey, _ uint64) (codexsdk.ConversationRecord, bool, codexsdk.TurnRecord, bool, error) {
	return codexsdk.ConversationRecord{}, false, codexsdk.TurnRecord{}, false, nil
}

func (s *chanConvStore) SaveTurns(_ context.Context, batch []codexsdk.TurnWrite) ([]codexsdk.TurnResult, error) {
	out := make([]codexsdk.TurnResult, len(batch))
	for i, w := range batch {
		out[i] = codexsdk.TurnResult{Conv: w.Conv, Turn: w.Rec, OK: true}
	}
	s.saved <- append([]codexsdk.TurnWrite(nil), batch...)
	return out, nil
}

// enqueueFirstTurn 新对话的首个请求：会话簿 Open 后把首轮记录入写入队列（尚未落盘）。
func enqueueFirstTurn(t *testing.T, book *codexsdk.ConversationBook, conv string) codexsdk.ConversationKey {
	t.Helper()
	key := codexsdk.NewConversationKey("cfg:1", 1, conv)
	id := book.Prepare(context.Background(), codexsdk.ConversationRequest{
		Key:            key,
		InstallationID: "inst-1",
		Input:          codexsdk.AnalyzeConversationInput([]byte(`{"model":"gpt-4o","input":"hi"}`)),
	})
	require.True(t, id.Turn.First, "新对话首个请求")
	return key
}

// TestConversationWorkerFlushOnClose C3-11：停机时在预算内最后 Flush 一次——节拍
// 设为 1h（运行期间不可能触发），Close 返回时已入队的首轮记录必须已交给存储。
func TestConversationWorkerFlushOnClose(t *testing.T) {
	store := newChanConvStore()
	book := codexsdk.NewConversationBook(store)
	w := NewConversationWorker(book, nil, ConversationWorkerConfig{Interval: time.Hour})
	require.Equal(t, "codex-conversation", w.Name())
	require.NoError(t, w.Start(context.Background()))
	require.Error(t, w.Start(context.Background()), "二次 Start 报错")

	key := enqueueFirstTurn(t, book, "conv-close")
	select {
	case <-store.saved:
		t.Fatal("节拍 1h：Close 前不应 Flush")
	default:
	}
	require.NoError(t, w.Close(context.Background()))
	select {
	case batch := <-store.saved:
		require.Len(t, batch, 1)
		require.Equal(t, key, batch[0].Key, "停机最后一次 Flush 落盘已入队的首轮记录")
	default:
		t.Fatal("Close 返回时必须已完成最后一次 Flush")
	}
	require.NoError(t, w.Close(context.Background()), "Close 幂等")
	require.Equal(t, int64(1), w.Stats().(conversationWorkerStats).ConvSaveTurnsCalls)
}

// TestConversationWorkerCloseWithoutStart 未 Start 的 Close 也最后 Flush 一次；
// 预算已到期则不再 Flush（停机预算内语义）。
func TestConversationWorkerCloseWithoutStart(t *testing.T) {
	store := newChanConvStore()
	book := codexsdk.NewConversationBook(store)
	enqueueFirstTurn(t, book, "conv-nostart")

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	w0 := NewConversationWorker(book, nil, ConversationWorkerConfig{})
	require.NoError(t, w0.Close(expired))
	require.Len(t, store.saved, 0, "预算已到期：不再 Flush")

	w := NewConversationWorker(book, nil, ConversationWorkerConfig{})
	require.NoError(t, w.Close(context.Background()))
	require.Len(t, store.saved, 1, "未 Start 的 Close 仍最后 Flush 一次")
}

// TestConversationWorkerTickerFlush 节拍驱动 Flush（间隔可注入）：入队后由 ticker
// 落盘——以存储收到批次为屏障。
func TestConversationWorkerTickerFlush(t *testing.T) {
	store := newChanConvStore()
	book := codexsdk.NewConversationBook(store)
	w := NewConversationWorker(book, nil, ConversationWorkerConfig{Interval: time.Millisecond})
	require.Equal(t, conversationFlushIntervalDefault, NewConversationWorker(book, nil, ConversationWorkerConfig{}).interval, "默认 50ms")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, w.Start(ctx))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	key := enqueueFirstTurn(t, book, "conv-tick")
	select {
	case batch := <-store.saved:
		require.Equal(t, key, batch[0].Key)
	case <-time.After(5 * time.Second):
		t.Fatal("ticker 未驱动 Flush")
	}
}

// TestConversationWorkerFlushNow FlushNow 同步排空（测试屏障）。
func TestConversationWorkerFlushNow(t *testing.T) {
	store := newChanConvStore()
	book := codexsdk.NewConversationBook(store)
	w := NewConversationWorker(book, nil, ConversationWorkerConfig{Interval: time.Hour})
	enqueueFirstTurn(t, book, "conv-now")
	w.FlushNow(context.Background())
	require.Len(t, store.saved, 1)
}

// TestConversationWorkerStatsKeys Stats 扁平键：conv_* 覆盖 ConversationStats 全部
// 字段 + conv_local_rejects，shape_* 覆盖 ShapeStats 全部字段（SDK 增字段 ⇒ 本用例
// 红，提醒同步 openapi 注释与 locales）；本地拒绝计数直出。
func TestConversationWorkerStatsKeys(t *testing.T) {
	book := codexsdk.NewConversationBook(newChanConvStore())
	w := NewConversationWorker(book, nil, ConversationWorkerConfig{})
	raw, err := json.Marshal(w.Stats())
	require.NoError(t, err)
	var got map[string]int64
	require.NoError(t, json.Unmarshal(raw, &got))

	want := map[string]bool{"conv_local_rejects": true}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[codexsdk.ConversationStats]()) {
		want["conv_"+snakeCase(f.Name)] = true
	}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[codexsdk.ShapeStats]()) {
		want["shape_"+snakeCase(f.Name)] = true
	}
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	require.Equal(t, want, keys)

	before := got["conv_local_rejects"]
	convLocalRejects.Add(1)
	raw, err = json.Marshal(w.Stats())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, before+1, got["conv_local_rejects"])
}

var snakeBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])|([A-Z]+)([A-Z][a-z])`)

// snakeCase Go 字段名 → json 蛇形键（WSUntrackedConnections → ws_untracked_connections）。
func snakeCase(s string) string {
	s = snakeBoundary.ReplaceAllString(s, "${1}${3}_${2}${4}")
	return strings.ToLower(s)
}
