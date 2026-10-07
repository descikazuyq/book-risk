package book

import (
	"strings"
	"testing"
)

// setupAmountLowerModifiedOrder 准备题述场景（初始现金 1000，未开日内亏损保护，
// 行情无缺口）：A、B 最大持仓量均为 100；最新已生效报价分别为 A 序号 1、时刻 10、
// 价格 10 与 B 序号 1、时刻 20、价格 20；账户总持仓金额上限先设为 200。
//
// 依次接受三张买单：
//   - 订单一（id1）：A 总量 3、限价 8，待成交；
//   - 订单二（id2）：B 总量 5、限价 20，先以 18 成交 2 份（部分成交、剩余 3）；
//   - 订单三（id3）：A 总量 2、限价 8，待成交；
//
// 第三张接受后，再把订单二修改为总量 4、限价 25（已成交 2 保持不变，修改后
// 有效剩余量为 2）。
//
// 修改后、再次下调上限前：
//   - 现金余额 964（2 份 B 按成交价 18 结算：1000-36），B 持仓 2；
//   - 现金占用 90，按各单【限价】计：订单一 3×8=24、订单三 2×8=16、
//     订单二修改后剩余 2×25=50；
//   - 持仓金额 40（B 2×20），买单剩余金额占用 100，按
//     【max(限价, 最新已生效报价)】计：订单一 3×10=30、订单三 2×10=20、
//     订单二 2×max(25,20)=50；合计 140。
//
// 两种金额口径不能混用：现金占用是 90 而金额上限占用是 100。
func setupAmountLowerModifiedOrder(t *testing.T) (e *Engine, id1, id2, id3 int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)
	if _, err := e.SetPositionAmountLimit(200); err != nil {
		t.Fatal(err)
	}

	id1 = mustBuy(t, e, "A", 3, 8)
	id2 = mustBuy(t, e, "B", 5, 20)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id2, Symbol: "B", Side: Buy, Price: 18, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	id3 = mustBuy(t, e, "A", 2, 8)
	// 第三张接受后才修改第二张：修改不改变它原先的接受次序（编号 2 仍在编号 3
	// 之前），只以新剩余量 2 与新限价 25 替换占用。
	if err := e.Modify(id2, 4, 25); err != nil {
		t.Fatal(err)
	}

	// 修改记录固化旧参数 5/20、新参数 4/25 与已成交 2。
	var modRec *Record
	recs := e.Records()
	for i := range recs {
		if recs[i].Kind == RecordModified && recs[i].OrderID == id2 {
			modRec = &recs[i]
		}
	}
	if modRec == nil {
		t.Fatalf("修改订单二必须追加一条修改记录")
	}
	if modRec.Qty != 4 || modRec.Limit != 25 || modRec.Filled != 2 || modRec.Remaining != 2 ||
		modRec.OldQty != 5 || modRec.OldLimit != 20 {
		t.Fatalf("修改记录内容错误: %+v", modRec)
	}

	// 下调前账实核对。
	if e.Cash() != 964 {
		t.Fatalf("下调前现金余额应为964: %d", e.Cash())
	}
	if e.Position("A") != 0 || e.Position("B") != 2 {
		t.Fatalf("下调前持仓错误: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	// 现金占用按限价：24 + 16 + 50 = 90。
	if e.ReservedCash() != 90 || e.AvailableCash() != 874 {
		t.Fatalf("下调前现金占用错误（按限价）: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}
	// 金额上限占用按 max(限价,报价)：30 + 20 + 50 = 100，合计 140。
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 200 ||
		st.Holding != 40 || st.BuyReserved != 100 || st.Total != 140 {
		t.Fatalf("下调前金额状态错误: %+v", st)
	}
	if o, _ := e.Order(id1); o.Status != StatusPending || o.Qty != 3 || o.Limit != 8 || o.Remaining() != 3 {
		t.Fatalf("订单一前置状态错误: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Qty != 4 || o.Limit != 25 ||
		o.Filled != 2 || o.Remaining() != 2 {
		t.Fatalf("订单二修改后状态错误（总量4限价25、已成交2、剩余2）: %+v", o)
	}
	if o, _ := e.Order(id3); o.Status != StatusPending || o.Qty != 2 || o.Limit != 8 || o.Remaining() != 2 {
		t.Fatalf("订单三前置状态错误: %+v", o)
	}
	return e, id1, id2, id3
}

// TestAmountLowerLimitModifiedOrderCancelsInAcceptanceOrderRegression 回归保障：
// 部分成交买单被修改总量与限价后，账户总持仓金额上限下调收敛时，撤单仍按订单
// 【原来的接受次序】（订单编号从大到小）进行，金额判断采用修改后的有效剩余量
// 与限价，撤销只释放当前未成交部分的占用，历史成交继续保留。
//
// 上限从 200 下调为 70：
//   - 先撤第三张（编号 3）全部 2 份：撤销前判断金额为持仓 40、买单占用 100、
//     合计 140；
//   - 再撤第二张（编号 2）修改后剩余的 2 份：修改不能让它排到第三张之后；
//     撤销前判断金额为持仓 40、买单占用 80、合计 120；被取消数量为 2，
//     不能写成原订单的剩余量 3，也不能撤掉它已成交的 2 份；
//   - 第一张（编号 1）仍待成交；合计 40+30=70 恰好降到上限即停止。
func TestAmountLowerLimitModifiedOrderCancelsInAcceptanceOrderRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountLowerModifiedOrder(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(70)
	if err != nil {
		t.Fatal(err)
	}
	// 返回编号顺序必须先第三张、再第二张；第一张不得被撤。
	if len(canceled) != 2 || canceled[0] != id3 || canceled[1] != id2 {
		t.Fatalf("应按原接受次序返回订单三、订单二，实际 %v", canceled)
	}

	// 本次调整只新增两条撤销记录，顺序与返回编号一致。
	var recs []Record
	for _, r := range e.Records()[recsBefore:] {
		if r.Kind != RecordCanceled {
			t.Fatalf("本次调整只能新增撤销记录，实际出现 %s: %+v", r.Kind, r)
		}
		if !r.AmtEnabled {
			t.Fatalf("下调上限造成的撤销记录必须携带金额快照: %+v", r)
		}
		recs = append(recs, r)
	}
	if len(recs) != 2 {
		t.Fatalf("应恰好新增两条撤销记录，实际 %d 条", len(recs))
	}

	// 第一条：撤订单三全部 2 份（未成交过）；保存撤销前判断金额 40/100/140。
	r3 := recs[0]
	if r3.OrderID != id3 || r3.Symbol != "A" || r3.Side != Buy ||
		r3.Qty != 2 || r3.Limit != 8 || r3.Filled != 0 || r3.Remaining != 2 {
		t.Fatalf("第一条撤销记录应关联订单三且取消数量为2: %+v", r3)
	}
	if !strings.Contains(r3.Reason, "账户总持仓金额上限") || !strings.Contains(r3.Reason, "超限") {
		t.Fatalf("撤销原因须注明账户金额超限: %q", r3.Reason)
	}
	if r3.AmtLimit != 70 || r3.AmtHolding != 40 || r3.AmtBuyReserved != 100 || r3.AmtTotal != 140 {
		t.Fatalf("第一条记录应固化撤销前金额（持仓40/买单100/合计140，上限70）: %+v", r3)
	}

	// 第二条：订单二按【修改后】有效剩余量取消 2 份（不是修改前剩余量 3），
	// 已成交 2 份保留；保存撤销前判断金额 40/80/120。
	r2 := recs[1]
	if r2.OrderID != id2 || r2.Symbol != "B" || r2.Side != Buy ||
		r2.Qty != 4 || r2.Limit != 25 || r2.Filled != 2 || r2.Remaining != 2 {
		t.Fatalf("第二条记录应关联订单二、保留总量4限价25与已成交2份、取消修改后剩余2份: %+v", r2)
	}
	if !strings.Contains(r2.Reason, "账户总持仓金额上限") || !strings.Contains(r2.Reason, "超限") {
		t.Fatalf("撤销原因须注明账户金额超限: %q", r2.Reason)
	}
	if r2.AmtLimit != 70 || r2.AmtHolding != 40 || r2.AmtBuyReserved != 80 || r2.AmtTotal != 120 {
		t.Fatalf("第二条记录应固化撤销前金额（持仓40/买单80/合计120，上限70）: %+v", r2)
	}

	// 两条记录参与判断的报价定位都保留 A、B 当时的序号、时刻和价格，按合约排列。
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

	// 订单三整单撤销；订单二撤销修改后剩余 2 份、累计成交 2 保留，订单仍保留
	// 修改后的总量 4、限价 25；订单一仍待成交，合计到 70 后立即停止。
	if o, _ := e.Order(id3); o.Status != StatusCanceled || o.Filled != 0 || o.Remaining() != 0 {
		t.Fatalf("订单三应整单撤销: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusCanceled || o.Qty != 4 || o.Limit != 25 ||
		o.Filled != 2 || o.Remaining() != 0 {
		t.Fatalf("订单二应保留总量4限价25与累计成交2、剩余量撤销为0: %+v", o)
	}
	if o, _ := e.Order(id1); o.Status != StatusPending || o.Qty != 3 || o.Limit != 8 ||
		o.Filled != 0 || o.Remaining() != 3 {
		t.Fatalf("合计降到70后必须停止，订单一保持待成交、总量3: %+v", o)
	}

	// 账实核对：撤单只释放未成交部分的冻结，现金余额与已成交持仓不变。
	if e.Cash() != 964 {
		t.Fatalf("撤单不得改动现金余额: %d", e.Cash())
	}
	if e.Position("A") != 0 || e.Position("B") != 2 {
		t.Fatalf("历史成交持仓必须保留: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	// 现金占用只剩订单一按限价冻结的 3×8=24。
	if e.ReservedCash() != 24 || e.AvailableCash() != 940 {
		t.Fatalf("现金占用应只剩24、可用940: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}
	// 金额查询：持仓 40、买单占用只剩订单一 3×max(8,10)=30、合计恰好 70。
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 70 ||
		st.Holding != 40 || st.BuyReserved != 30 || st.Total != 70 {
		t.Fatalf("金额合计应降为70且不再超限: %+v", st)
	}
}

// TestAmountLowerLimitModifiedOrderAtCurrentTotalNoCancelRegression 边界：上限只
// 下调到当前合计 140（恰好不超限）时不撤任何单、不新增记录，修改后的订单二与
// 其他订单、现金及金额占用全部保留。
func TestAmountLowerLimitModifiedOrderAtCurrentTotalNoCancelRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountLowerModifiedOrder(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(140)
	if err != nil {
		t.Fatal(err)
	}
	if len(canceled) != 0 {
		t.Fatalf("上限恰为当前合计140时不应撤单，实际 %v", canceled)
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("不撤单时不得新增任何记录，实际新增 %d 条", len(e.Records())-recsBefore)
	}

	if o, _ := e.Order(id1); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("订单一必须保留: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Qty != 4 || o.Limit != 25 ||
		o.Filled != 2 || o.Remaining() != 2 {
		t.Fatalf("订单二必须保留修改后的部分成交状态: %+v", o)
	}
	if o, _ := e.Order(id3); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("订单三必须保留: %+v", o)
	}
	if st := e.PositionAmountStatus(); st.Limit != 140 ||
		st.Holding != 40 || st.BuyReserved != 100 || st.Total != 140 {
		t.Fatalf("金额状态应保留为40/100/140: %+v", st)
	}
	if e.Cash() != 964 || e.ReservedCash() != 90 {
		t.Fatalf("资金必须保留: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}
	if e.Position("B") != 2 {
		t.Fatalf("持仓必须保留: B=%d", e.Position("B"))
	}
}

// TestAmountLowerLimitModifiedOrderNegativeErrorsAndPreservesRegression 边界：
// 在含修改订单的状态下提交负上限必须报错，且原上限 200、三张订单（含订单二
// 修改后的总量 4、限价 25 与累计成交 2）及现金/金额占用全部保留，不新增记录。
func TestAmountLowerLimitModifiedOrderNegativeErrorsAndPreservesRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountLowerModifiedOrder(t)

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

	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 200 ||
		st.Holding != 40 || st.BuyReserved != 100 || st.Total != 140 {
		t.Fatalf("负上限报错后原上限与金额占用必须保留: %+v", st)
	}
	if o, _ := e.Order(id1); o.Status != StatusPending || o.Qty != 3 || o.Limit != 8 || o.Remaining() != 3 {
		t.Fatalf("订单一必须保留: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Qty != 4 || o.Limit != 25 ||
		o.Filled != 2 || o.Remaining() != 2 {
		t.Fatalf("订单二修改后的状态必须保留: %+v", o)
	}
	if o, _ := e.Order(id3); o.Status != StatusPending || o.Qty != 2 || o.Limit != 8 || o.Remaining() != 2 {
		t.Fatalf("订单三必须保留: %+v", o)
	}
	if e.Cash() != 964 || e.ReservedCash() != 90 || e.AvailableCash() != 874 {
		t.Fatalf("负上限报错后资金必须保留: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 0 || e.Position("B") != 2 {
		t.Fatalf("负上限报错后持仓必须保留: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
}
