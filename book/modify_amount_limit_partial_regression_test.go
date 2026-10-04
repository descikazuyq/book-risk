package book

import (
	"strings"
	"testing"
)

// 本文件为“修改部分成交买单时受账户总持仓金额上限约束”补充回归保障。
//
// 题述场景：现金、合约持仓限额均充足且行情连续时，账户已有 A 合约买单
// （总量 10、限价 12，已按价格 9 成交 4，A 最新有效报价为 10），另有 B 合约
// 未成交买单（数量 2、限价及有效报价均为 20）。账户总持仓金额上限为 200：
// 已持仓金额 40、未成交买单占用 112（A 剩余 6×max(12,10)=72，B 2×20=40），
// 合计 152。把 A 订单改为总量 12、限价 20 时，只能以剩余 8 的新占用替换该单
// 原来剩余 6 的占用（申请后合计 40+40+8×20=240），超过上限必须拒绝：
// 已成交的 4 股只计入持仓金额、不得再算进买单占用，也绝不能靠撤销 B 单腾额度。
//
// 回归点：
//   - 这次调用只增加一条拒绝记录，关联原订单，保留申请的总量与限价、判断时的
//     上限 200、两类金额（40/112）、修改前合计 152 与申请后合计 240，以及参与
//     判断的 A、B 报价定位（合约、序号、时刻、价格，按合约排列且不重复）。
//   - 拒绝后 A 订单仍是总量 10、限价 12、已成交 4、剩余 6；B 订单继续有效；
//     现金余额、买单现金占用、持仓与金额合计全部保持调用前的值。
//   - 拒绝记录固化的是拒绝发生时的事实：随后更新报价、调高上限并成功修改同一
//     订单后，旧记录仍保留 152、240、上限 200 与原报价定位；查询返回的记录或
//     其报价副本被篡改也不影响再次查询得到的历史内容。
//   - 申请后合计恰好等于上限的修改仍按已有规则允许，且不产生金额超限拒绝记录。

// setupModifyAmountPartial 构建题述前置状态并返回引擎与 A、B 买单编号：
// 现金 1_000_000，A、B 持仓限额均为 1000；A seq1（时刻 10）报价 10、
// B seq1（时刻 20）报价 20；A 买单总量 10 限价 12 已按 9 成交 4，
// B 买单 2 股限价 20 未成交；金额上限设为 200（设置时合计 152，不撤任何单）。
func setupModifyAmountPartial(t *testing.T) (e *Engine, aID, bID int64) {
	t.Helper()
	e, _ = NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustSetMax(t, e, "B", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	aID = mustBuy(t, e, "A", 10, 12) // 现金占用 120
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: aID, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatalf("A 单部分成交 4 股失败: %v", err)
	}

	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)
	bID = mustBuy(t, e, "B", 2, 20) // 现金占用 40

	// 上限 200：持仓 40 + A 剩余 72 + B 40 = 152，设置时不得撤单。
	canceled, err := e.SetPositionAmountLimit(200)
	if err != nil {
		t.Fatalf("设置金额上限 200 失败: %v", err)
	}
	if len(canceled) != 0 {
		t.Fatalf("合计 152 ≤ 200，设置上限不应撤单，实际撤销 %v", canceled)
	}
	return e, aID, bID
}

// assertPartialPreState 校验题述前置金额构成：已持仓 40、买单占用 112、合计 152。
func assertPartialPreState(t *testing.T, e *Engine) {
	t.Helper()
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Limit != 200 || st.Holding != 40 || st.BuyReserved != 112 || st.Total != 152 {
		t.Fatalf("前置金额构成错误: %+v（应为持仓 40 + 买单占用 112 = 152）", st)
	}
	// 现金：初始 1_000_000 扣成交 4×9=36；买单现金占用 6×12+2×20=112。
	if e.Cash() != 999_964 || e.ReservedCash() != 112 || e.AvailableCash() != 999_852 {
		t.Fatalf("前置资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("前置持仓错误: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
}

// assertRejectQuoteRefs 校验报价定位恰好是 A seq1（时刻 10）价 10 与
// B seq1（时刻 20）价 20：按合约升序排列、无重复。
func assertRejectQuoteRefs(t *testing.T, refs []RiskQuoteRef) {
	t.Helper()
	want := []RiskQuoteRef{
		{Symbol: "A", Seq: 1, Moment: 10, Price: 10},
		{Symbol: "B", Seq: 1, Moment: 20, Price: 20},
	}
	if len(refs) != len(want) {
		t.Fatalf("参与判断的报价定位应有 A、B 两条且不重复，实际 %+v", refs)
	}
	for i, w := range want {
		if refs[i] != w {
			t.Fatalf("第 %d 条报价定位错误: got=%+v want=%+v（全部: %+v）", i, refs[i], w, refs)
		}
	}
}

// TestModifyPartialBuyAmountLimitRejectSnapshotAndOccupancy 覆盖核心拒绝路径：
// 改单申请后合计 240 > 200 被拒，拒绝记录能说明当时为何拒绝，且拒绝不改动
// 原订单与 B 订单的任何占用。
func TestModifyPartialBuyAmountLimitRejectSnapshotAndOccupancy(t *testing.T) {
	e, aID, bID := setupModifyAmountPartial(t)
	assertPartialPreState(t, e)

	cashBefore := e.Cash()
	reservedBefore := e.ReservedCash()
	availableBefore := e.AvailableCash()
	posABefore, posBBefore := e.Position("A"), e.Position("B")
	stBefore := e.PositionAmountStatus()

	recsBefore := len(e.Records())
	err := e.Modify(aID, 12, 20)
	if err == nil {
		t.Fatal("申请后合计 240 > 上限 200，修改必须拒绝")
	}
	// 错误本身要能说明为何拒绝：上限、修改前与申请后合计都在文案中。
	for _, s := range []string{"账户总持仓金额上限", "200", "152", "240", "不撤销其他订单"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("拒绝错误 %q 应包含 %q", err.Error(), s)
		}
	}

	// 这次调用只增加一条记录，且是关联原订单的拒绝记录。
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("金额超限修改只能新增一条记录，实际新增 %d 条", len(recs)-recsBefore)
	}
	rj := recs[recsBefore]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rj)
	}
	if rj.OrderID != aID || rj.Symbol != "A" || rj.Side != Buy {
		t.Fatalf("拒绝记录必须关联原 A 买单: %+v", rj)
	}
	if linked, ok := e.Order(rj.OrderID); !ok || linked.Symbol != "A" {
		t.Fatalf("拒绝记录的订单编号必须能关联到原订单: id=%d", rj.OrderID)
	}
	// 保留申请的总量与限价（注意是申请值 12/20，不是订单现值）。
	if rj.Qty != 12 || rj.Limit != 20 {
		t.Fatalf("拒绝记录必须保留申请的总量 12 与限价 20: qty=%d limit=%d", rj.Qty, rj.Limit)
	}
	if rj.Reason == "" || !strings.Contains(rj.Reason, "240") {
		t.Fatalf("拒绝记录必须说明具体原因与申请后合计: %q", rj.Reason)
	}
	// 拒绝发生时该合约的报价快照同样固化。
	if !rj.QuoteValid || rj.QuoteSeq != 1 || rj.QuoteMoment != 10 || rj.QuotePrice != 10 ||
		!rj.MaxPositionValid || rj.MaxPosition != 1_000 {
		t.Fatalf("拒绝记录应固化 A 的报价与持仓限额快照: %+v", rj)
	}

	// 金额快照：判断时上限、两类金额、修改前合计与申请后合计。
	if !rj.AmtEnabled || rj.AmtLimit != 200 ||
		rj.AmtHolding != 40 || rj.AmtBuyReserved != 112 ||
		rj.AmtTotal != 152 || rj.AmtApplyTotal != 240 {
		t.Fatalf("拒绝记录金额快照错误: limit=%d holding=%d reserved=%d total=%d apply=%d",
			rj.AmtLimit, rj.AmtHolding, rj.AmtBuyReserved, rj.AmtTotal, rj.AmtApplyTotal)
	}
	// 已成交 4 股只在持仓金额中计一次：买单占用 112 若把已成交数量也算入将是 152。
	if rj.AmtHolding+rj.AmtBuyReserved != rj.AmtTotal {
		t.Fatalf("持仓与买单占用合计必须自洽: %d+%d != %d",
			rj.AmtHolding, rj.AmtBuyReserved, rj.AmtTotal)
	}
	assertRejectQuoteRefs(t, rj.AmtQuoteRefs)

	// A 订单保持调用前参数：总量 10、限价 12、已成交 4、剩余 6、部分成交。
	o, _ := e.Order(aID)
	if o.Qty != 10 || o.Limit != 12 || o.Filled != 4 || o.Remaining() != 6 || o.Status != StatusPartial {
		t.Fatalf("拒绝后 A 订单必须保持总量 10/限价 12/已成交 4/剩余 6: %+v", o)
	}
	// 绝不能通过撤销 B 订单腾出额度。
	ob, _ := e.Order(bID)
	if ob.Status != StatusPending || ob.Qty != 2 || ob.Limit != 20 || ob.Filled != 0 || ob.Remaining() != 2 {
		t.Fatalf("B 订单必须继续有效且参数不变: %+v", ob)
	}

	// 现金余额、买单现金占用、可用现金、持仓与金额合计全部保持调用前的值。
	if e.Cash() != cashBefore || e.ReservedCash() != reservedBefore || e.AvailableCash() != availableBefore {
		t.Fatalf("拒绝不得改变资金: cash=%d reserved=%d available=%d（调用前 %d/%d/%d）",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), cashBefore, reservedBefore, availableBefore)
	}
	if e.Position("A") != posABefore || e.Position("B") != posBBefore {
		t.Fatalf("拒绝不得改变持仓: A=%d B=%d（调用前 %d/%d）",
			e.Position("A"), e.Position("B"), posABefore, posBBefore)
	}
	if st := e.PositionAmountStatus(); st != stBefore {
		t.Fatalf("拒绝不得改变金额合计: after=%+v before=%+v", st, stBefore)
	}

	// 新增的唯一记录不能是撤销记录（显式排除任何隐式腾额度行为）。
	for _, r := range recs[recsBefore:] {
		if r.Kind == RecordCanceled {
			t.Fatalf("金额上限拒绝修改不得撤销任何订单: %+v", r)
		}
	}
}

// TestModifyPartialBuyAmountLimitRejectRecordFrozenAfterChange 覆盖拒绝记录的
// 事实固化：查询副本篡改、后续报价更新、调高上限并成功修改同一订单，都不能
// 改写旧记录的 152/240/上限 200 与原报价定位。
func TestModifyPartialBuyAmountLimitRejectRecordFrozenAfterChange(t *testing.T) {
	e, aID, bID := setupModifyAmountPartial(t)
	assertPartialPreState(t, e)

	recsBefore := len(e.Records())
	if err := e.Modify(aID, 12, 20); err == nil {
		t.Fatal("前置修改必须被拒绝")
	}
	oldIdx := recsBefore

	// 历史定位：在追加新记录后仍应能且只能找到这一条 240 拒绝记录。
	findOld := func(t *testing.T) Record {
		t.Helper()
		var found []Record
		for _, r := range e.Records() {
			if r.Kind == RecordRejected && r.OrderID == aID && r.AmtEnabled && r.AmtApplyTotal == 240 {
				found = append(found, r)
			}
		}
		if len(found) != 1 {
			t.Fatalf("旧拒绝记录必须恰好保留一条，实际 %d 条", len(found))
		}
		return found[0]
	}
	assertOld := func(t *testing.T) {
		t.Helper()
		old := findOld(t)
		if old.AmtLimit != 200 || old.AmtHolding != 40 || old.AmtBuyReserved != 112 ||
			old.AmtTotal != 152 || old.AmtApplyTotal != 240 || old.Qty != 12 || old.Limit != 20 {
			t.Fatalf("旧拒绝记录必须保留 152/240/上限 200 与申请参数: %+v", old)
		}
		assertRejectQuoteRefs(t, old.AmtQuoteRefs)
	}
	assertOld(t)

	// 篡改查询返回的记录及其报价信息副本，不得影响再次查询得到的历史内容。
	mut := e.Records()
	mut[oldIdx].AmtLimit = 1
	mut[oldIdx].AmtHolding = 1
	mut[oldIdx].AmtBuyReserved = 1
	mut[oldIdx].AmtTotal = 1
	mut[oldIdx].AmtApplyTotal = 1
	mut[oldIdx].Qty = 1
	mut[oldIdx].Limit = 1
	mut[oldIdx].AmtQuoteRefs[0] = RiskQuoteRef{Symbol: "X", Seq: 99, Moment: 99, Price: 99}
	mut[oldIdx].AmtQuoteRefs[1].Price = 99
	assertOld(t)

	// 随后更新报价：A seq2（时刻 11）价 8、B seq2（时刻 21）价 15。
	// 持仓 32 + A 剩余 6×max(12,8)=72 + B 2×max(20,15)=40 = 144 ≤ 200，不撤单。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 11, Price: 8}, 1)
	mustQuote(t, e, "B", Quote{Seq: 2, Moment: 21, Price: 15}, 1)
	if o, _ := e.Order(bID); o.Status != StatusPending {
		t.Fatalf("报价更新不应撤销 B 订单: %+v", o)
	}
	if o, _ := e.Order(aID); o.Status != StatusPartial || o.Qty != 10 || o.Limit != 12 {
		t.Fatalf("报价更新不应改变被拒 A 订单: %+v", o)
	}
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 2 || q.Price != 8 {
		t.Fatalf("前置：A 最新报价应已推进到 seq2 价 8: %+v ok=%v", q, ok)
	}
	assertOld(t) // 旧记录仍是 seq1 时刻 10 价 10 的原报价定位。

	// 调高金额上限到 1000，随后把同一订单成功改为总量 12、限价 20：
	// 持仓 32 + A 剩余 8×max(20,8)=160 + B 40 = 232 ≤ 1000。
	canceled, err := e.SetPositionAmountLimit(1000)
	if err != nil {
		t.Fatalf("调高上限失败: %v", err)
	}
	if len(canceled) != 0 {
		t.Fatalf("调高上限不得撤单: %v", canceled)
	}
	if err := e.Modify(aID, 12, 20); err != nil {
		t.Fatalf("调高上限后同一修改应成功: %v", err)
	}
	o, _ := e.Order(aID)
	if o.Qty != 12 || o.Limit != 20 || o.Filled != 4 || o.Remaining() != 8 || o.Status != StatusPartial {
		t.Fatalf("成功修改后 A 订单应为总量 12/限价 20/已成交 4/剩余 8: %+v", o)
	}
	// 成功修改只追加一条 RecordModified，记录修改前参数。
	var mod Record
	mods := 0
	for _, r := range e.Records() {
		if r.Kind == RecordModified && r.OrderID == aID {
			mods++
			mod = r
		}
	}
	if mods != 1 || mod.OldQty != 10 || mod.OldLimit != 12 || mod.Qty != 12 || mod.Limit != 20 ||
		mod.Filled != 4 || mod.Remaining != 8 {
		t.Fatalf("成功修改应恰好追加一条前后参数正确的修改记录: mods=%d rec=%+v", mods, mod)
	}
	// B 订单与资金按成功修改后的规则变化，不影响历史记录。
	if ob, _ := e.Order(bID); ob.Status != StatusPending {
		t.Fatalf("B 订单全程有效: %+v", ob)
	}
	if e.ReservedCash() != 200 { // A 新占用 8×20=160 + B 40
		t.Fatalf("成功修改后买单现金占用应为 200: %d", e.ReservedCash())
	}

	// 旧拒绝记录仍保留拒绝发生时的全部事实。
	assertOld(t)
}

// TestModifyPartialBuyAmountLimitExactEqualAllowed 覆盖边界：申请后合计恰好
// 等于上限（200）的修改按已有规则允许，且不产生金额超限拒绝记录。
func TestModifyPartialBuyAmountLimitExactEqualAllowed(t *testing.T) {
	e, aID, bID := setupModifyAmountPartial(t)
	assertPartialPreState(t, e)

	// 总量维持 10、限价提到 20：剩余 6×20=120 替换原剩余 6×12=72，
	// 申请后合计 = 152-72+120 = 200，恰好等于上限。
	recsBefore := len(e.Records())
	if err := e.Modify(aID, 10, 20); err != nil {
		t.Fatalf("申请后合计恰好等于上限应允许，实际拒绝: %v", err)
	}
	recs := e.Records()
	if len(recs) != recsBefore+1 || recs[recsBefore].Kind != RecordModified {
		t.Fatalf("恰好等于上限应只追加一条修改记录，实际: %d 条", len(recs)-recsBefore)
	}
	if r := recs[recsBefore]; r.OldLimit != 12 || r.Limit != 20 || r.Qty != 10 || r.Filled != 4 {
		t.Fatalf("边界修改记录参数错误: %+v", r)
	}

	o, _ := e.Order(aID)
	if o.Qty != 10 || o.Limit != 20 || o.Filled != 4 || o.Remaining() != 6 || o.Status != StatusPartial {
		t.Fatalf("边界允许后 A 订单状态错误: %+v", o)
	}
	// 金额合计恰好 200：持仓 40 + A 剩余占用 120 + B 40。
	st := e.PositionAmountStatus()
	if st.Holding != 40 || st.BuyReserved != 160 || st.Total != 200 {
		t.Fatalf("边界修改后金额合计应为 200: %+v", st)
	}
	// 买单现金占用同样只替换本单：112-72+120 = 160。
	if e.ReservedCash() != 160 {
		t.Fatalf("边界修改后买单现金占用应为 160: %d", e.ReservedCash())
	}
	if ob, _ := e.Order(bID); ob.Status != StatusPending || ob.Remaining() != 2 {
		t.Fatalf("B 订单不受影响: %+v", ob)
	}

	// 全部历史中都不应存在金额超限拒绝记录。
	for _, r := range e.Records() {
		if r.Kind == RecordRejected && r.AmtEnabled && r.AmtApplyTotal != 0 {
			t.Fatalf("恰好等于上限不得产生金额超限拒绝记录: %+v", r)
		}
	}
}
