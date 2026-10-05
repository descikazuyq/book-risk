package book

import (
	"strings"
	"testing"
)

// setupAmountLowerDescending 准备题述场景（初始现金 1000，未开日内亏损保护，行情
// 连续）：A、B 最大持仓量均为 100；有效报价分别为 A 序号 1、时刻 10、价格 10 与
// B 序号 1、时刻 20、价格 20；账户总持仓金额上限 200。
// 依次接受三张买单：
//   - 订单一：A 总量 5、限价 12，先按 9 成交 2（剩余 3）；
//   - 订单二：B 总量 4、限价 15，先按 14 成交 1（剩余 3）；
//   - 订单三：A 总量 2、限价 8，尚未成交（剩余 2）。
//
// 下调前：现金 968、现金占用 97（始终按限价计算）；已持仓金额 40（A 2×10、
// B 1×20），买单剩余金额占用 116（订单一 3×max(12,10)=36、订单二
// 3×max(15,20)=60、订单三 2×max(8,10)=20），合计 156。
func setupAmountLowerDescending(t *testing.T) (e *Engine, id1, id2, id3 int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)
	if _, err := e.SetPositionAmountLimit(200); err != nil {
		t.Fatal(err)
	}

	id1 = mustBuy(t, e, "A", 5, 12)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id1, Symbol: "A", Side: Buy, Price: 9, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	id2 = mustBuy(t, e, "B", 4, 15)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: id2, Symbol: "B", Side: Buy, Price: 14, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	id3 = mustBuy(t, e, "A", 2, 8)

	if e.Cash() != 968 || e.ReservedCash() != 97 || e.AvailableCash() != 871 {
		t.Fatalf("下调前资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("下调前持仓错误: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 200 ||
		st.Holding != 40 || st.BuyReserved != 116 || st.Total != 156 {
		t.Fatalf("下调前金额状态错误: %+v", st)
	}
	if o, _ := e.Order(id1); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 3 {
		t.Fatalf("订单一前置状态错误: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 3 {
		t.Fatalf("订单二前置状态错误: %+v", o)
	}
	if o, _ := e.Order(id3); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("订单三前置状态错误: %+v", o)
	}
	return e, id1, id2, id3
}

// findAmountCancelRecords 返回本次新增（recsBefore 之后）的金额上限撤销记录。
func findAmountCancelRecords(t *testing.T, e *Engine, recsBefore int) []Record {
	t.Helper()
	var out []Record
	for _, r := range e.Records()[recsBefore:] {
		if r.Kind != RecordCanceled {
			t.Fatalf("本次调整只能新增撤销记录，实际出现 %s: %+v", r.Kind, r)
		}
		if !r.AmtEnabled {
			t.Fatalf("下调上限造成的撤销记录必须携带金额快照: %+v", r)
		}
		out = append(out, r)
	}
	return out
}

// TestAmountLowerLimitCancelsDescendingWholeRemainingRegression 保护既有功能：
// 上限从 200 下调到 76 时，金额额度按“持仓金额 + 有效买单剩余金额占用”计算
// （不是现金冻结额度：现金占用仍按限价），按订单编号从大到小整单撤销全部剩余量，
// 一旦合计不再超限立即停止——绝不为凑到上限只撤销一张订单的一部分，也不能继续
// 撤销第一张。
//
// 订单三（剩余 2，金额占用 20）先撤：156-20=136 仍超限；订单二再撤其未成交 3 份
// （金额占用 60），保留已成交 1 份：136-60=76 恰好等于上限，停止；订单一保持
// 部分成交、剩余 3 份不动。撤销只释放冻结，现金余额与已成交持仓不变。
func TestAmountLowerLimitCancelsDescendingWholeRemainingRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountLowerDescending(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(76)
	if err != nil {
		t.Fatal(err)
	}
	// 返回编号顺序必须与实际撤销一致：先订单三、再订单二；订单一不得被撤。
	if len(canceled) != 2 || canceled[0] != id3 || canceled[1] != id2 {
		t.Fatalf("应按编号从大到小返回订单三、订单二，实际 %v", canceled)
	}

	// 本次调整只新增两条撤销记录。
	recs := findAmountCancelRecords(t, e, recsBefore)
	if len(recs) != 2 {
		t.Fatalf("应恰好新增两条撤销记录，实际 %d 条", len(recs))
	}

	// 第一条：撤订单三全部 2 份（未成交过）；保存撤销前判断金额 40/116/156。
	r3 := recs[0]
	if r3.OrderID != id3 || r3.Symbol != "A" || r3.Side != Buy ||
		r3.Qty != 2 || r3.Filled != 0 || r3.Remaining != 2 {
		t.Fatalf("第一条撤销记录应关联订单三且取消数量等于实际剩余量 2: %+v", r3)
	}
	if !strings.Contains(r3.Reason, "账户总持仓金额上限") || !strings.Contains(r3.Reason, "超限") {
		t.Fatalf("撤销原因须说明账户金额超限: %q", r3.Reason)
	}
	if r3.AmtLimit != 76 || r3.AmtHolding != 40 || r3.AmtBuyReserved != 116 || r3.AmtTotal != 156 {
		t.Fatalf("第一条记录应固化撤销前金额（持仓40/买单116/合计156，上限76）: %+v", r3)
	}

	// 第二条：订单二只取消未成交 3 份并保留已成交 1 份；保存撤销前判断金额
	// 40/96/136（撤掉订单三的 20 后，买单占用 116-20=96）。
	r2 := recs[1]
	if r2.OrderID != id2 || r2.Symbol != "B" || r2.Side != Buy ||
		r2.Qty != 4 || r2.Filled != 1 || r2.Remaining != 3 {
		t.Fatalf("第二条撤销记录应关联订单二、保留已成交1份、取消剩余3份: %+v", r2)
	}
	if !strings.Contains(r2.Reason, "账户总持仓金额上限") || !strings.Contains(r2.Reason, "超限") {
		t.Fatalf("撤销原因须说明账户金额超限: %q", r2.Reason)
	}
	if r2.AmtLimit != 76 || r2.AmtHolding != 40 || r2.AmtBuyReserved != 96 || r2.AmtTotal != 136 {
		t.Fatalf("第二条记录应固化撤销前金额（持仓40/买单96/合计136，上限76）: %+v", r2)
	}

	// 两条记录的报价定位都保留 A、B 当时的序号、时刻和价格，按合约排列各一条。
	wantRefs := []RiskQuoteRef{
		{Symbol: "A", Seq: 1, Moment: 10, Price: 10},
		{Symbol: "B", Seq: 1, Moment: 20, Price: 20},
	}
	for _, r := range recs {
		if len(r.AmtQuoteRefs) != 2 {
			t.Fatalf("每条撤销记录都应保存 A、B 两条报价定位: %+v", r.AmtQuoteRefs)
		}
		for i, want := range wantRefs {
			if r.AmtQuoteRefs[i] != want {
				t.Fatalf("报价定位[%d]错误: 期望 %+v，实际 %+v", i, want, r.AmtQuoteRefs[i])
			}
		}
	}

	// 订单三整单撤销；订单二撤销剩余 3 份、已成交 1 份保留；
	// 订单一仍为部分成交、剩余 3 份，合计到 76 后必须立即停止、不得再撤。
	if o, _ := e.Order(id3); o.Status != StatusCanceled || o.Filled != 0 || o.Remaining() != 0 {
		t.Fatalf("订单三应整单撤销: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusCanceled || o.Filled != 1 || o.Remaining() != 0 {
		t.Fatalf("订单二应只撤销剩余3份并保留已成交1份: %+v", o)
	}
	if o, _ := e.Order(id1); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 3 {
		t.Fatalf("合计降到76后必须停止，订单一保持部分成交、剩余3份: %+v", o)
	}

	// 账实核对：现金余额不变；现金占用按限价释放 2×8+3×15=61，97-61=36；
	// A、B 持仓分别为 2、1；金额合计恰为 76（持仓40 + 订单一3×12=36）。
	if e.Cash() != 968 {
		t.Fatalf("撤单只释放冻结不得改动现金余额: %d", e.Cash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("已成交持仓必须保留: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	if e.ReservedCash() != 36 || e.AvailableCash() != 932 {
		t.Fatalf("现金占用应降为36、可用现金932: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 76 ||
		st.Holding != 40 || st.BuyReserved != 36 || st.Total != 76 {
		t.Fatalf("金额合计应降为76且不再超限: %+v", st)
	}
}

// TestAmountLowerLimitExactlyAtBoundaryStopsRegression 直接边界：相同初始状态下把
// 上限改为 136，只撤销编号最大的订单三（释放金额占用 20），合计 156-20=136
// 恰好等于上限便立即停止；订单二、订单一都保持有效。
func TestAmountLowerLimitExactlyAtBoundaryStopsRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountLowerDescending(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(136)
	if err != nil {
		t.Fatal(err)
	}
	if len(canceled) != 1 || canceled[0] != id3 {
		t.Fatalf("恰好136即停止时应只撤销订单三，实际 %v", canceled)
	}

	recs := findAmountCancelRecords(t, e, recsBefore)
	if len(recs) != 1 {
		t.Fatalf("应只新增一条撤销记录，实际 %d 条", len(recs))
	}
	if recs[0].OrderID != id3 || recs[0].Remaining != 2 {
		t.Fatalf("唯一撤销记录应关联订单三、取消其实际剩余2份: %+v", recs[0])
	}
	if recs[0].AmtLimit != 136 || recs[0].AmtHolding != 40 ||
		recs[0].AmtBuyReserved != 116 || recs[0].AmtTotal != 156 {
		t.Fatalf("撤销记录应保存撤销前判断金额（上限136）: %+v", recs[0])
	}

	if o, _ := e.Order(id3); o.Status != StatusCanceled {
		t.Fatalf("订单三应已撤销: %+v", o)
	}
	// 恰好等于上限即停止：订单二仍为部分成交、剩余 3；订单一仍剩余 3。
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 3 {
		t.Fatalf("合计恰好136时订单二必须保留: %+v", o)
	}
	if o, _ := e.Order(id1); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 3 {
		t.Fatalf("合计恰好136时订单一必须保留: %+v", o)
	}
	// 买单金额占用 36+60=96，合计 40+96=136；现金占用按限价 36+45=81。
	if st := e.PositionAmountStatus(); st.Limit != 136 ||
		st.Holding != 40 || st.BuyReserved != 96 || st.Total != 136 {
		t.Fatalf("合计应恰好136: %+v", st)
	}
	if e.Cash() != 968 || e.ReservedCash() != 81 || e.AvailableCash() != 887 {
		t.Fatalf("资金核对错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("持仓不得变化: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
}

// TestAmountLowerLimitNegativeErrorsBeforeAnyCancelRegression 直接边界：负上限必须
// 报错，且不能先撤单再报错——原上限 200、三张订单及其资金/金额占用全部保留，
// 不新增任何记录。
func TestAmountLowerLimitNegativeErrorsBeforeAnyCancelRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountLowerDescending(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(-1)
	if err == nil {
		t.Fatal("负上限必须报错")
	}
	if !strings.Contains(err.Error(), "不能为负") {
		t.Fatalf("错误应说明上限不能为负: %v", err)
	}
	if canceled != nil {
		t.Fatalf("报错时不得返回任何撤销编号: %v", canceled)
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("负上限报错不得先撤单或新增任何记录，实际新增 %d 条",
			len(e.Records())-recsBefore)
	}

	// 原上限 200 保留。
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 200 ||
		st.Holding != 40 || st.BuyReserved != 116 || st.Total != 156 {
		t.Fatalf("负上限报错后原上限与金额占用必须保留: %+v", st)
	}
	// 三张订单全部保持调用前状态。
	if o, _ := e.Order(id1); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 3 {
		t.Fatalf("订单一必须保留: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 3 {
		t.Fatalf("订单二必须保留: %+v", o)
	}
	if o, _ := e.Order(id3); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("订单三必须保留: %+v", o)
	}
	// 资金与持仓占用全部保留。
	if e.Cash() != 968 || e.ReservedCash() != 97 || e.AvailableCash() != 871 {
		t.Fatalf("负上限报错后资金必须保留: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("负上限报错后持仓必须保留: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
}
