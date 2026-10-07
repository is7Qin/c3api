// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// BatchRow credit 取批单行（usage_logs 未记账正收益子集投影，§5.1/§5.2）。
type BatchRow struct {
	ID        int64
	UID       int64
	Cost      int64
	Earn      int64
	CreatedAt time.Time
}

// UIDCredit 单供应商本批记账增量（§5.2 阶段 C 输入；uid 升序 = 锁序）。
type UIDCredit struct {
	UID         int64
	Earn        int64 // Σ earn（毫分）
	ToFrozen    int64 // 入 chunks 额（freeze_hours>0 时 = Earn；=0 时不写 chunks）
	ToAvailable int64 // 直接入 available 额
}

// DayRecon 源日封账增量（§3.10；维度 = 源行 created_at 的 UTC 日）。
type DayRecon struct {
	UID       int64
	SourceDay time.Time // UTC 日（truncate 到日）
	GrossCost int64
	Earn      int64
	Rows      int64
}

// AggregateBatch 按 uid 聚合 Σearn 与源日封账增量（§5.2）：
//   - 入参唯一性断言（批内 id 不得重复）；
//   - 溢出即失败闭合（BIGINT 范围，绝不静默钳制应付款）；
//   - freezeByUID[uid] > 0 ⇒ to_frozen = Σearn（写 chunks）；否则 to_available = Σearn；
//   - 金额守恒（M2）：Σto_available + Σto_frozen == Σ rows.Earn，不符返回错误；
//   - 返回 uid 升序（显式锁序）。
func AggregateBatch(rows []BatchRow, freezeByUID map[int64]int) ([]UIDCredit, []DayRecon, error) {
	seen := make(map[int64]struct{}, len(rows))
	type acc struct{ earn, cost int64 }
	perUID := make(map[int64]*acc)
	perDay := make(map[[2]int64]*DayRecon)
	var rowsEarn int64

	for _, r := range rows {
		if _, dup := seen[r.ID]; dup {
			return nil, nil, fmt.Errorf("supplier: duplicate batch row id %d", r.ID)
		}
		seen[r.ID] = struct{}{}
		if r.Earn < 0 || r.Cost < 0 {
			return nil, nil, fmt.Errorf("supplier: negative earn/cost in row %d", r.ID)
		}
		var err error
		if rowsEarn, err = addChecked(rowsEarn, r.Earn); err != nil {
			return nil, nil, err
		}
		a := perUID[r.UID]
		if a == nil {
			a = &acc{}
			perUID[r.UID] = a
		}
		if a.earn, err = addChecked(a.earn, r.Earn); err != nil {
			return nil, nil, err
		}
		if a.cost, err = addChecked(a.cost, r.Cost); err != nil {
			return nil, nil, err
		}
		day := r.CreatedAt.UTC().Truncate(24 * time.Hour)
		key := [2]int64{r.UID, day.Unix()}
		d := perDay[key]
		if d == nil {
			d = &DayRecon{UID: r.UID, SourceDay: day}
			perDay[key] = d
		}
		if d.GrossCost, err = addChecked(d.GrossCost, r.Cost); err != nil {
			return nil, nil, err
		}
		if d.Earn, err = addChecked(d.Earn, r.Earn); err != nil {
			return nil, nil, err
		}
		if d.Rows, err = addChecked(d.Rows, 1); err != nil {
			return nil, nil, err
		}
	}

	totals := make([]UIDCredit, 0, len(perUID))
	var sumAvail, sumFrozen int64
	for uid, a := range perUID {
		uc := UIDCredit{UID: uid, Earn: a.earn}
		if freezeByUID[uid] > 0 {
			uc.ToFrozen = a.earn
		} else {
			uc.ToAvailable = a.earn
		}
		var err error
		if sumAvail, err = addChecked(sumAvail, uc.ToAvailable); err != nil {
			return nil, nil, err
		}
		if sumFrozen, err = addChecked(sumFrozen, uc.ToFrozen); err != nil {
			return nil, nil, err
		}
		totals = append(totals, uc)
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i].UID < totals[j].UID })

	// 金额守恒（M2）：Σto_available + Σto_frozen == Σ rows.Earn。
	combined, err := addChecked(sumAvail, sumFrozen)
	if err != nil {
		return nil, nil, err
	}
	if combined != rowsEarn {
		return nil, nil, fmt.Errorf("supplier: credit conservation violation: avail+frozen=%d != earn=%d", combined, rowsEarn)
	}

	recon := make([]DayRecon, 0, len(perDay))
	for _, d := range perDay {
		recon = append(recon, *d)
	}
	sort.Slice(recon, func(i, j int) bool {
		if recon[i].UID != recon[j].UID {
			return recon[i].UID < recon[j].UID
		}
		return recon[i].SourceDay.Before(recon[j].SourceDay)
	})
	return totals, recon, nil
}

// addChecked int64 加法溢出检查（失败闭合，不静默钳制）。
func addChecked(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("supplier: int64 overflow summing %d + %d", a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("supplier: int64 underflow summing %d + %d", a, b)
	}
	return a + b, nil
}
