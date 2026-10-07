package book

import (
	"strings"
	"testing"
)

// setupAmountModifiedBuyOrder 准备题述场景（初始现金 1000，未开日内亏损保护，行情
// 连续）：A、B 最大持仓量均为 100；有效报价分别为 A 序号 1、时刻 10、价格 10 与
// B 序号 1、时刻 20、价格 20；账户总持仓金额上限 200。
// 依次接受三张买单：
//   - 订单一：A 总量 3、限价 8，尚未成交（剩余 3）；
//   - 订单二：B 总量 5、限价 20，先按 18 成交 2（剩余 3）；
//   - 订单三：A 总量 2、限价 8，尚未成交（剩余 2）。
//
// 第三张接受后，再把订单二改为总量 4、限价 25：已成交 2 份不动，有效剩余量变为
// 2（4−2），而不是改单前的剩余 3。
//
// 修改后（下调上限前）：
//   - 现金 964（1000−18×2）；B 持仓 2；现金占用 90，始终按限价计算：
//     订单一 3×8=24、订单二修改后剩余 2×25=50、订单三 2×8=16；
//   - 持仓金额 40（B 2×20，A 无持仓）；买单剩余金额占用 100，按
//     剩余量 × max(限价, 已生效报价) 计算：
//     订单一 3×max(8,10)=30、订单二 2×max(25,20)=50、订单三 2×max(8,10)=20；
//   - 合计 40+100=140。
//
// 两种金额口径不能混用：现金占用恒按限价（90），金额上限占用按限价与报价较高者
// （100）。修改只更新订单二参数，不改变其在 B 合约买单接受序列中的位置。
func setupAmountModifiedBuyOrder(t *testing.T) (e *Engine, id1, id2, id3 int64) {
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
	// 第三张接受后才修改第二张：改为总量 4、限价 25，有效剩余量 4−2=2。
	mustModify(t, e, id2, 4, 25)

	// 资金：现金只按成交价扣减 36；现金占用始终按（新）限价与有效剩余量。
	if e.Cash() != 964 || e.ReservedCash() != 90 || e.AvailableCash() != 874 {
		t.Fatalf("下调前资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 0 || e.Position("B") != 2 {
		t.Fatalf("下调前持仓错误: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 200 ||
		st.Holding != 40 || st.BuyReserved != 100 || st.Total != 140 {
		t.Fatalf("下调前金额状态错误: %+v", st)
	}
	if o, _ := e.Order(id1); o.Status != StatusPending || o.Filled != 0 ||
		o.Qty != 3 || o.Limit != 8 || o.Remaining() != 3 {
		t.Fatalf("订单一前置状态错误: %+v", o)
	}
	// 订单二保留修改后的总量 4、限价 25，累计成交 2，有效剩余量 2。
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Filled != 2 ||
		o.Qty != 4 || o.Limit != 25 || o.Remaining() != 2 {
		t.Fatalf("订单二前置状态错误: %+v", o)
	}
	if o, _ := e.Order(id3); o.Status != StatusPending || o.Filled != 0 ||
		o.Qty != 2 || o.Limit != 8 || o.Remaining() != 2 {
		t.Fatalf("订单三前置状态错误: %+v", o)
	}
	return e, id1, id2, id3
}

// findModifiedBuyAmountCancelRecords 返回本次新增（recsBefore 之后）的金额上限
// 撤销记录，并校验本次调整只能新增携带金额快照的撤销记录。
func findModifiedBuyAmountCancelRecords(t *testing.T, e *Engine, recsBefore int) []Record {
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

// TestAmountLowerLimitModifiedBuyKeepsAcceptOrderRegression 保护既有功能：部分成交
// 买单修改总量与限价后，金额判断必须采用“修改后的有效剩余量与限价”，而下限下调
// 的撤单次序仍沿用订单最初的接受次序（按订单编号从大到小）——修改第二张不能让它
// 排到第三张之后。
//
// 上限从 200 下调到 70：
//   - 先撤订单三全部 2 份（A，未成交过）：撤销前合计 140（持仓40、买单占用100）；
//   - 再撤订单二修改后仍剩余的 2 份：撤销前合计 120（持仓40、买单占用80），
//     被取消数量必须是修改后的有效剩余量 2，绝不能写成改单前的剩余量 3；
//   - 合计降到 40+30=70 恰好等于上限，立即停止，订单一保持待成交。
//
// 撤单只按新限价释放当前未成交部分的现金占用：订单三 2×8=16、订单二 2×25=50，
// 共释放 66，现金占用 90−66=24（只剩订单一 3×8）；现金余额与 B 的 2 份历史成交
// 继续保留，订单二仍保存修改后的总量 4、限价 25 与累计成交 2。
func TestAmountLowerLimitModifiedBuyKeepsAcceptOrderRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountModifiedBuyOrder(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(70)
	if err != nil {
		t.Fatal(err)
	}
	// 返回编号顺序必须与实际撤销一致：先订单三、再订单二；订单一不得被撤。
	// 若错误地按“修改时间”排序，订单二会被排到订单三之前，本断言立即失败。
	if len(canceled) != 2 || canceled[0] != id3 || canceled[1] != id2 {
		t.Fatalf("应按原接受次序（编号从大到小）返回订单三、订单二，实际 %v", canceled)
	}

	recs := findModifiedBuyAmountCancelRecords(t, e, recsBefore)
	if len(recs) != 2 {
		t.Fatalf("应恰好新增两条撤销记录，实际 %d 条", len(recs))
	}

	wantRefs := []RiskQuoteRef{
		{Symbol: "A", Seq: 1, Moment: 10, Price: 10},
		{Symbol: "B", Seq: 1, Moment: 20, Price: 20},
	}

	// 第一条：撤订单三全部 2 份；保存撤销前判断金额 40/100/140（上限 70）。
	r3 := recs[0]
	if r3.OrderID != id3 || r3.Symbol != "A" || r3.Side != Buy ||
		r3.Qty != 2 || r3.Filled != 0 || r3.Remaining != 2 {
		t.Fatalf("第一条撤销记录应关联订单三且取消数量等于实际剩余量 2: %+v", r3)
	}
	if !strings.Contains(r3.Reason, "账户总持仓金额上限") || !strings.Contains(r3.Reason, "超限") {
		t.Fatalf("撤销原因须说明账户金额超限: %q", r3.Reason)
	}
	if r3.AmtLimit != 70 || r3.AmtHolding != 40 || r3.AmtBuyReserved != 100 || r3.AmtTotal != 140 {
		t.Fatalf("第一条记录应固化撤销前金额（持仓40/买单100/合计140，上限70）: %+v", r3)
	}

	// 第二条：订单二只取消修改后仍未成交的 2 份并保留已成交 2 份；被取消数量
	// 必须是 2，不能写成原订单修改前的剩余量 3；保存撤销前判断金额
	// 40/80/120（撤掉订单三的 20 后，买单占用 100−20=80）。
	r2 := recs[1]
	if r2.OrderID != id2 || r2.Symbol != "B" || r2.Side != Buy ||
		r2.Qty != 4 || r2.Limit != 25 || r2.Filled != 2 || r2.Remaining != 2 {
		t.Fatalf("第二条撤销记录应关联订单二（总量4、限价25）、保留已成交2份、取消修改后剩余2份: %+v", r2)
	}
	if !strings.Contains(r2.Reason, "账户总持仓金额上限") || !strings.Contains(r2.Reason, "超限") {
		t.Fatalf("撤销原因须说明账户金额超限: %q", r2.Reason)
	}
	if r2.AmtLimit != 70 || r2.AmtHolding != 40 || r2.AmtBuyReserved != 80 || r2.AmtTotal != 120 {
		t.Fatalf("第二条记录应固化撤销前金额（持仓40/买单80/合计120，上限70）: %+v", r2)
	}

	// 两条记录的报价定位都保留 A、B 当时的序号、时刻和价格，按合约排列各一条。
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

	// 订单三整单撤销；订单二撤销修改后剩余的 2 份、已成交 2 份保留，订单本身
	// 仍保存修改后的总量 4、限价 25；订单一待成交、剩余 3，合计到 70 后必须
	// 立即停止、不得再撤。
	if o, _ := e.Order(id3); o.Status != StatusCanceled || o.Filled != 0 || o.Remaining() != 0 {
		t.Fatalf("订单三应整单撤销: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusCanceled || o.Filled != 2 ||
		o.Qty != 4 || o.Limit != 25 || o.Remaining() != 0 {
		t.Fatalf("订单二应只撤销修改后剩余2份、保留已成交2份，且保留总量4限价25: %+v", o)
	}
	if o, _ := e.Order(id1); o.Status != StatusPending || o.Filled != 0 ||
		o.Qty != 3 || o.Limit != 8 || o.Remaining() != 3 {
		t.Fatalf("合计降到70后必须停止，订单一保持待成交、剩余3份: %+v", o)
	}

	// 账实核对：撤单只释放未成交部分的冻结，现金余额与历史成交不变。
	// 现金占用按限价释放 2×8+2×25=66，90−66=24（只剩订单一 3×8=24）；
	// 金额口径合计为持仓 40 + 订单一 3×max(8,10)=30，恰好 70。
	if e.Cash() != 964 {
		t.Fatalf("撤单只释放冻结不得改动现金余额: %d", e.Cash())
	}
	if e.Position("A") != 0 || e.Position("B") != 2 {
		t.Fatalf("已成交持仓必须保留: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	if e.ReservedCash() != 24 || e.AvailableCash() != 940 {
		t.Fatalf("现金占用应降为24、可用现金940: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 70 ||
		st.Holding != 40 || st.BuyReserved != 30 || st.Total != 70 {
		t.Fatalf("金额合计应降为70且不再超限: %+v", st)
	}
}

// TestAmountLowerLimitModifiedBuyAtCurrentTotalNoCancelRegression 直接边界：上限只
// 下调到当前合计 140（140 恰好等于合计）时不撤任何单，三张订单及各自（含修改后）
// 的资金与金额占用全部保留，不新增撤销记录。
func TestAmountLowerLimitModifiedBuyAtCurrentTotalNoCancelRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountModifiedBuyOrder(t)

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

	// 新上限 140 生效，但金额占用与合计不变。
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 140 ||
		st.Holding != 40 || st.BuyReserved != 100 || st.Total != 140 {
		t.Fatalf("上限改为140后金额状态错误: %+v", st)
	}
	// 三张订单全部保持调用前状态（订单二仍为修改后的参数）。
	if o, _ := e.Order(id1); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 3 {
		t.Fatalf("订单一必须保留: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Filled != 2 ||
		o.Qty != 4 || o.Limit != 25 || o.Remaining() != 2 {
		t.Fatalf("订单二必须保留修改后参数: %+v", o)
	}
	if o, _ := e.Order(id3); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("订单三必须保留: %+v", o)
	}
	// 资金与持仓占用全部保留。
	if e.Cash() != 964 || e.ReservedCash() != 90 || e.AvailableCash() != 874 {
		t.Fatalf("资金必须保留: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 0 || e.Position("B") != 2 {
		t.Fatalf("持仓必须保留: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
}

// TestAmountLowerLimitModifiedBuyNegativeErrorsBeforeAnyCancelRegression 直接边界：
// 负上限必须报错，且不能先撤单再报错——原上限 200、三张订单（含订单二的修改结果）
// 及其资金/金额占用全部保留，不新增任何记录。
func TestAmountLowerLimitModifiedBuyNegativeErrorsBeforeAnyCancelRegression(t *testing.T) {
	e, id1, id2, id3 := setupAmountModifiedBuyOrder(t)

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
		st.Holding != 40 || st.BuyReserved != 100 || st.Total != 140 {
		t.Fatalf("负上限报错后原上限与金额占用必须保留: %+v", st)
	}
	// 三张订单全部保持调用前状态。
	if o, _ := e.Order(id1); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 3 {
		t.Fatalf("订单一必须保留: %+v", o)
	}
	if o, _ := e.Order(id2); o.Status != StatusPartial || o.Filled != 2 ||
		o.Qty != 4 || o.Limit != 25 || o.Remaining() != 2 {
		t.Fatalf("订单二必须保留修改后参数: %+v", o)
	}
	if o, _ := e.Order(id3); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("订单三必须保留: %+v", o)
	}
	// 资金与持仓占用全部保留。
	if e.Cash() != 964 || e.ReservedCash() != 90 || e.AvailableCash() != 874 {
		t.Fatalf("负上限报错后资金必须保留: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 0 || e.Position("B") != 2 {
		t.Fatalf("负上限报错后持仓必须保留: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
}
