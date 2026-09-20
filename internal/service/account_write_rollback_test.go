// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// failingAccountWriteStore 让账号写失败（模拟事务回滚），其余能力全部委托给
// fakeStore——只替换写动词，故断言的是"写失败后服务层的失效动作面"。
type failingAccountWriteStore struct {
	*fakeStore
	err error
}

func (f *failingAccountWriteStore) UpdateAccountsBatch(context.Context, []int64, repository.AccountPatch) ([]repository.AccountWriteResult, error) {
	return nil, f.err
}

// TestAccountWriteFailureLeavesNoInvalidation 钉住 I8 的时序边界：组级重载 /
// clients 失效 / NOTIFY 只在写**成功提交后**执行。写失败（回滚）路径不得留下
// 半套失效——否则快照会按未落库的值重建，客户端连接缓存被无谓作废。
func TestAccountWriteFailureLeavesNoInvalidation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(*Service, int64, repository.AccountPatch) error
	}{
		{"patch", func(s *Service, id int64, p repository.AccountPatch) error {
			_, err := s.PatchAccount(ctx, id, p, nil)
			return err
		}},
		{"batch", func(s *Service, id int64, p repository.AccountPatch) error {
			_, err := s.UpdateAccountsBatch(ctx, []int64{id}, p)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			tpl, err := fs.CreateTemplate(ctx, &domain.Template{
				Name: "t", BaseURL: "https://t.example.com",
				SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			})
			require.NoError(t, err)
			acc, err := fs.CreateAccount(ctx, &domain.Account{
				Name: "acc", TemplateID: tpl.ID, UpstreamKey: "sk-1", MaxConcurrency: 8, Enabled: true,
			})
			require.NoError(t, err)

			rec, pr := &invRecorder{}, &pubRecorder{}
			svc := &Service{store: &failingAccountWriteStore{fakeStore: fs, err: repository.ErrNotFound}, inv: rec, pub: pr, log: nil}

			key := "sk-rotated"
			name := "renamed"
			require.Error(t, tc.call(svc, acc.ID, repository.AccountPatch{UpstreamKey: &key, Name: &name}))

			require.Zero(t, rec.total(), "a rolled-back write must not mark any snapshot dirty")
			require.Zero(t, pr.total(), "a rolled-back write must not publish NOTIFY")
			// 旧值保持不变（无半套写入）。
			got, err := fs.GetAccount(ctx, acc.ID)
			require.NoError(t, err)
			require.Equal(t, "acc", got.Name)
			require.Equal(t, "sk-1", got.UpstreamKey)
		})
	}
}
