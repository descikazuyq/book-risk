package book

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// 本文件为“交易日已开启后，SetPositionAmountLimit 因账户金额合计（持仓市值 +
// 有效买单剩余量占用）超出 int64 范围而整体失败”补充回归保障：金额占用与日内
// 净值是两种口径，金额口径越界不能连累净值口径——拒绝记录必须固化当时完整的
// 日内风险快照（日号、基准净值、当前净值、亏损、亏损上限、是否已限制增险），
// 与开日后的其他拒绝记录保持一致；同时记录中的金额上限启用状态与上限值保留
// 调用前口径，原因说明申请值与金额越界，不能把申请值写成已经生效的上限。
//
// 数值构造：初始现金 1000；合约 A 报价 10 买入并成交 10 份后开启日号 7、亏损
// 上限 100 的交易日（基准净值 1000）；A 下一条连续报价降到 9：现金 900、
// A 持仓市值 90，当前净值 990、亏损 10，尚未限制增险。合约 B 已有合法持仓
// 限额，最新已生效报价为 MaxInt64、没有持仓；金额上限尚未启用时接受 B 限价 1、
// 数量 2 的买单，只占用 2 现金（现金口径），金额口径的买单占用按
// max(限价 1, 报价 MaxInt64) = MaxInt64、共 2 份而无法表示。
// 此时申请把金额上限设为 100：当前金额合计已不可表示，设置必须整体失败。

// amountSetOverflowRiskSetup 构造上述场景：返回引擎、A 的买单编号与 B 的买单编号。
func amountSetOverflowRiskSetup(t *testing.T) (e *Engine, aID, bID int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	aID = mustBuy(t, e, "A", 10, 10) // 占用 100 现金
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: aID, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.Position("A") != 10 {
		t.Fatalf("A 成交后前置状态错误: cash=%d reserved=%d posA=%d", e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	// 开启日号 7、亏损上限 100 的交易日：基准净值 1000。
	if err := e.StartTradingDay(7, 100); err != nil {
		t.Fatal(err)
	}
	// A 的下一条连续报价降到 9：净值 990、亏损 10，尚未限制。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1)
	st := e.RiskStatus()
	if !st.Open || st.Day != 7 || st.Baseline != 1000 || st.Equity != 990 ||
		st.Loss != 10 || st.LossLimit != 100 || st.Restricted {
		t.Fatalf("A 报价回落后的前置风险状态错误: %+v", st)
	}

	// B 最新已生效报价为 MaxInt64、没有持仓；金额上限尚未启用。
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: math.MaxInt64}, 1)
	if e.PositionAmountStatus().Enabled {
		t.Fatal("本场景要求申请前金额上限尚未启用")
	}
	bID = mustBuy(t, e, "B", 2, 1) // 现金口径只占用 2；金额口径 2×MaxInt64 无法表示
	if e.Cash() != 900 || e.ReservedCash() != 2 {
		t.Fatalf("B 买单只应占用 2 现金: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}
	if o, _ := e.Order(bID); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("B 买单应为待成交、有效剩余 2: %+v", o)
	}
	return e, aID, bID
}

// TestSetAmountLimitOverflowAfterOpenKeepsRiskSnapshot 覆盖主场景：开日后设置金额
// 上限因金额占用越界而整体失败——返回可用 errors.Is 识别的 ErrInt64Overflow，
// 只新增一条拒绝记录，不产生触线或撤单记录，不启用申请的金额上限；现金、持仓、
// 订单累计成交量、有效剩余量与占用均保持调用前状态；拒绝记录准确保留日号 7 与
// 当时的风险数值（基准 1000、净值 990、亏损 10、上限 100、未限制），金额上限
// 快照保留调用前口径（未启用、零值），原因同时说明申请值与金额越界。
func TestSetAmountLimitOverflowAfterOpenKeepsRiskSnapshot(t *testing.T) {
	e, aID, bID := amountSetOverflowRiskSetup(t)

	cash, reserved, available := e.Cash(), e.ReservedCash(), e.AvailableCash()
	posA, posB := e.Position("A"), e.Position("B")
	riskBefore := e.RiskStatus()
	aBefore, _ := e.Order(aID)
	bBefore, _ := e.Order(bID)
	recordsBefore := append([]Record(nil), e.Records()...)

	canceled, err := e.SetPositionAmountLimit(100)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("金额越界必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if canceled != nil {
		t.Fatalf("整体失败不得返回任何撤单编号，实际 %v", canceled)
	}

	// 不启用申请的金额上限，也不保留申请值 100。
	if st := e.PositionAmountStatus(); st.Enabled {
		t.Fatalf("越界后申请的金额上限不得生效: %+v", st)
	}

	// 现金、持仓保持调用前状态。
	if e.Cash() != cash || e.ReservedCash() != reserved || e.AvailableCash() != available {
		t.Fatalf("失败不得改变现金/占用/可用: cash=%d(before %d) reserved=%d(before %d) available=%d(before %d)",
			e.Cash(), cash, e.ReservedCash(), reserved, e.AvailableCash(), available)
	}
	if e.Position("A") != posA || e.Position("B") != posB {
		t.Fatalf("失败不得改变持仓: posA=%d(before %d) posB=%d(before %d)",
			e.Position("A"), posA, e.Position("B"), posB)
	}
	// 订单累计成交量、状态与有效剩余量保持调用前状态（B 不被代替撤单）。
	aAfter, _ := e.Order(aID)
	bAfter, _ := e.Order(bID)
	if aAfter != aBefore || bAfter != bBefore {
		t.Fatalf("失败不得改变任何订单: A before=%+v after=%+v; B before=%+v after=%+v",
			aBefore, aAfter, bBefore, bAfter)
	}
	if bAfter.Status != StatusPending || bAfter.Reason != "" || bAfter.Remaining() != 2 {
		t.Fatalf("B 买单必须仍为有效待成交单: %+v", bAfter)
	}

	// 风险状态不变：仍是日号 7、亏损 10、未触线；且不得产生触线或撤单记录。
	if rs := e.RiskStatus(); rs != riskBefore {
		t.Fatalf("失败不得改变风险状态: before=%+v after=%+v", riskBefore, rs)
	}
	recs := e.Records()
	for _, r := range recs {
		if r.Kind == RecordRiskTriggered {
			t.Fatalf("金额越界失败不得产生触线记录: %+v", r)
		}
		if r.Kind == RecordCanceled {
			t.Fatalf("金额越界失败不得产生撤单记录: %+v", r)
		}
	}

	// 只新增一条拒绝记录，先前的记录原样保留。
	if len(recs) != len(recordsBefore)+1 {
		t.Fatalf("只能新增一条拒绝记录，实际新增 %d 条", len(recs)-len(recordsBefore))
	}
	if !reflect.DeepEqual(recordsBefore, recs[:len(recordsBefore)]) {
		t.Fatal("失败不得改写先前的记录")
	}
	rj := recs[len(recordsBefore)]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝，实际 %s", rj.Kind)
	}
	// 完整日内风险快照：日号 7、基准 1000、当前净值 990、亏损 10、上限 100、
	// 尚未限制增险——不能表现为未开日或零净值。
	if rj.RiskDay != 7 || rj.RiskBaseline != 1000 || rj.RiskEquity != 990 ||
		rj.RiskLoss != 10 || rj.RiskLimit != 100 || rj.RiskRestrict {
		t.Fatalf("拒绝记录必须保存当时完整的日内风险快照: day=%d baseline=%d equity=%d loss=%d limit=%d restrict=%v",
			rj.RiskDay, rj.RiskBaseline, rj.RiskEquity, rj.RiskLoss, rj.RiskLimit, rj.RiskRestrict)
	}
	// 金额上限快照保留调用前口径：尚未启用、上限为零；申请值 100 不得写成已生效上限。
	if rj.AmtEnabled || rj.AmtLimit != 0 {
		t.Fatalf("记录中的金额上限状态必须保留调用前口径（未启用、零值）: enabled=%v limit=%d",
			rj.AmtEnabled, rj.AmtLimit)
	}
	if rj.AmtHolding != 0 || rj.AmtBuyReserved != 0 || rj.AmtTotal != 0 ||
		rj.AmtApplyTotal != 0 || rj.AmtQuoteRefs != nil {
		t.Fatalf("金额合计不可表示时只留启用状态与上限，不得携带伪金额: %+v", rj)
	}
	// 原因仍说明申请值与金额越界。
	if !strings.Contains(rj.Reason, "100") || !strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因必须说明申请值 100 与金额超出 int64 范围: %q", rj.Reason)
	}
}

// TestSetAmountLimitOverflowSnapshotNotRewrittenByLaterEvents 验证已保存的拒绝
// 记录不被后续报价、亏损上限调整或开启新交易日改写；记录中始终是拒绝发生时
// 日号 7 的风险快照。
func TestSetAmountLimitOverflowSnapshotNotRewrittenByLaterEvents(t *testing.T) {
	e, _, _ := amountSetOverflowRiskSetup(t)

	if _, err := e.SetPositionAmountLimit(100); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("前置设置必须因金额越界失败: %v", err)
	}

	// 后续 A 报价继续回落：亏损扩大，但不得改写已保存记录的净值 990/亏损 10。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 8}, 1) // 净值 980、亏损 20
	// 调整亏损上限（20 恰好达限，触发触线与撤单）。
	if err := e.SetLossLimit(20); err != nil {
		t.Fatal(err)
	}
	// 开启新的交易日。
	if err := e.StartTradingDay(8, 100); err != nil {
		t.Fatal(err)
	}

	// 在记录序列中定位当时新增的拒绝记录（唯一一条原因含申请值 100 的设置失败）。
	var found []Record
	for _, r := range e.Records() {
		if strings.Contains(r.Reason, "设置账户总持仓金额上限为 100") {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("场景中只应有一条该拒绝记录，实际 %d 条", len(found))
	}
	rj := found[0]
	if rj.RiskDay != 7 || rj.RiskBaseline != 1000 || rj.RiskEquity != 990 ||
		rj.RiskLoss != 10 || rj.RiskLimit != 100 || rj.RiskRestrict {
		t.Fatalf("后续行情/调限额/跨日不得改写已保存的日号 7 风险快照: %+v", rj)
	}
}

// TestSetAmountLimitOverflowBeforeOpenKeepsZeroRiskSnapshot 覆盖未开日口径：
// 同类金额越界失败继续保留零值风险快照，不自动开日；其余整体失败规则不变。
func TestSetAmountLimitOverflowBeforeOpenKeepsZeroRiskSnapshot(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: math.MaxInt64}, 1)
	bID := mustBuy(t, e, "B", 2, 1) // 金额口径 2×MaxInt64 无法表示

	cash, reserved := e.Cash(), e.ReservedCash()
	recordsBefore := len(e.Records())

	if _, err := e.SetPositionAmountLimit(100); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("金额越界必须返回 ErrInt64Overflow，实际 %v", err)
	}
	if e.RiskStatus().Open {
		t.Fatal("未开日时的失败不得自动开启交易日")
	}
	if st := e.PositionAmountStatus(); st.Enabled {
		t.Fatal("越界后申请的金额上限不得生效")
	}
	if e.Cash() != cash || e.ReservedCash() != reserved {
		t.Fatalf("失败不得改变现金/占用: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}
	if o, _ := e.Order(bID); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("B 买单必须仍为有效待成交单: %+v", o)
	}
	recs := e.Records()
	if len(recs) != recordsBefore+1 || recs[recordsBefore].Kind != RecordRejected {
		t.Fatal("只能新增一条拒绝记录")
	}
	rj := recs[recordsBefore]
	if rj.RiskDay != 0 || rj.RiskBaseline != 0 || rj.RiskEquity != 0 ||
		rj.RiskLoss != 0 || rj.RiskLimit != 0 || rj.RiskRestrict {
		t.Fatalf("未开日时拒绝记录必须保留零值风险快照: %+v", rj)
	}
	for _, r := range recs {
		if r.Kind == RecordRiskTriggered || r.Kind == RecordCanceled {
			t.Fatalf("未开日失败不得产生触线或撤单记录: %+v", r)
		}
	}
}
