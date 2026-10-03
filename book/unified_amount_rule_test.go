package book

import "testing"

// 本文件通过公开入口固定“买单未成交部分持仓金额占用”的统一口径，确保该规则在
// 新买单、修改买单与账户金额汇总三处共用同一实现：持仓金额占用按
// 未成交数量 × max(限价, 最新已生效报价)，与始终按限价计的现金占用区分；
// 已成交数量只计入持仓金额，未成交卖单不提前抵减持仓金额。
//
// 题述数字：剩余 5 份、限价 8、最新报价 12 时，现金占用 40、持仓金额占用 60；
// 报价降到 6 且订单仍有效时，现金仍占用 40，持仓金额占用按限价计为 40。
func TestUnifiedBuyAmountRuleCashVsPositionAmount(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 12}, 1)
	if _, err := e.SetPositionAmountLimit(1_000_000); err != nil {
		t.Fatal(err)
	}

	id := mustBuy(t, e, "A", 5, 8) // 剩余 5、限价 8、报价 12
	if e.ReservedCash() != 40 {
		t.Fatalf("现金占用按限价计应为 40，实际 %d", e.ReservedCash())
	}
	if st := e.PositionAmountStatus(); st.Holding != 0 || st.BuyReserved != 60 || st.Total != 60 {
		t.Fatalf("持仓金额占用应按 max(8,12)=12 计为 60: %+v", st)
	}

	// 报价降到 6、订单仍有效：现金占用不变，持仓金额占用回落到按限价 8 计的 40。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 6}, 1)
	if o, _ := e.Order(id); o.Status != StatusPending || o.Remaining() != 5 {
		t.Fatalf("报价回落不得撤单或改剩余量: %+v", o)
	}
	if e.ReservedCash() != 40 {
		t.Fatalf("报价回落不得改变现金占用，应仍为 40，实际 %d", e.ReservedCash())
	}
	if st := e.PositionAmountStatus(); st.BuyReserved != 40 || st.Total != 40 {
		t.Fatalf("报价 6 下持仓金额占用应按限价 8 计为 40: %+v", st)
	}
}

// 部分成交买单修改：总量 10、已成交 4 改成总量 8、限价 12，在最新报价 10 下，
// 新剩余量 4 的现金与持仓金额占用均为 48；历史成交的现金与持仓不重新计算。
func TestUnifiedBuyAmountRuleModifyPartialFillSpec(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	id := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(1_000_000); err != nil {
		t.Fatal(err)
	}
	// 修改前：持仓 4×10=40，剩余 6×max(10,10)=60，现金占用 60。
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 60 || st.Total != 100 {
		t.Fatalf("修改前金额状态错误: %+v", st)
	}

	mustModify(t, e, id, 8, 12) // 新剩余 4、限价 12、报价 10
	if e.ReservedCash() != 48 {
		t.Fatalf("修改后现金占用应为剩余 4×限价 12=48，实际 %d", e.ReservedCash())
	}
	if e.Cash() != 1_000_000-40 || e.Position("A") != 4 {
		t.Fatalf("历史成交的现金与持仓不得因修改重算: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 48 || st.Total != 88 {
		t.Fatalf("剩余 4 的持仓金额占用应 max(12,10)×4=48，持仓仍 40: %+v", st)
	}

	// 已成交 4 份只计入持仓，不再留在买单剩余量中。
	if o, _ := e.Order(id); o.Filled != 4 || o.Qty != 8 || o.Remaining() != 4 {
		t.Fatalf("已成交数量不得留在买单剩余量: %+v", o)
	}
}

// 未成交卖单不能提前抵减持仓金额：挂出卖单前后持仓金额合计不变。
func TestUnifiedBuyAmountRuleUnfilledSellDoesNotOffset(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	id := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(1_000_000); err != nil {
		t.Fatal(err)
	}
	before := e.PositionAmountStatus()
	if before.Holding != 100 || before.Total != 100 {
		t.Fatalf("前置持仓金额应为 100: %+v", before)
	}
	mustSell(t, e, "A", 6, 10) // 成交前不抵减
	after := e.PositionAmountStatus()
	if after != before {
		t.Fatalf("未成交卖单不得提前抵减持仓金额: before=%+v after=%+v", before, after)
	}
}
