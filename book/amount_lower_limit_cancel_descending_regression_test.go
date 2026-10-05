package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“下调账户总持仓金额上限”（SetPositionAmountLimit）补充自动化回归保障：
// 保护已有买单部分成交后，下调上限时按订单编号从大到小撤销各单的全部剩余量、
// 合计不再超限即停止的现有功能。金额额度按“持仓金额 + 有效买单剩余金额占用”
// 计算（买单剩余量按 max(限价, 最新报价) 占用，与现金按限价冻结是两套口径），
// 绝不能为凑到上限只撤销一张订单的一部分。
//
// 固定场景（未开启日内亏损保护，行情连续）：
//   - 初始现金 1000；A、B 最大持仓量均为 100。
//   - 有效报价：A 序号1、时刻10、价格10；B 序号1、时刻20、价格20。
//   - 金额上限先设为 200。
//   - 订单1：A 买单总量 5、限价 12，先按价格 9 成交 2（持仓 A=2）。
//   - 订单2：B 买单总量 4、限价 15，先按价格 14 成交 1（持仓 B=1）。
//   - 订单3：A 买单总量 2、限价 8，尚未成交。
//
// 下调前口径逐笔固化：现金 968（=1000−2×9−1×14），现金占用 97
// （订单1 剩余3×限价12 + 订单2 剩余3×限价15 + 订单3 2×限价8 = 36+45+16），
// 可用现金 871；已持仓金额 40（A 2×10 + B 1×20），买单剩余金额占用 116
// （3×max(12,10)=36 + 3×max(15,20)=60 + 2×max(8,10)=20），合计 156。
// 注意 B 买单与第三张 A 买单的金额占用按报价计算，现金占用仍按限价计算。

// lowerAmountSetup 构造下调上限前的共用前置状态，返回引擎与三张买单编号
// （bid1：A 部分成交；bid2：B 部分成交；bid3：A 待成交，编号依次为 1、2、3）。
func lowerAmountSetup(t *testing.T) (e *Engine, bid1, bid2, bid3 int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)

	if _, err := e.SetPositionAmountLimit(200); err != nil {
		t.Fatalf("前置设置上限 200 不应报错: %v", err)
	}

	bid1 = mustBuy(t, e, "A", 5, 12)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid1, Symbol: "A", Side: Buy, Price: 9, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	bid2 = mustBuy(t, e, "B", 4, 15)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bid2, Symbol: "B", Side: Buy, Price: 14, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	bid3 = mustBuy(t, e, "A", 2, 8)

	// 新引擎首三张买单编号依次为 1、2、3，题面按编号叙述撤销顺序。
	if bid1 != 1 || bid2 != 2 || bid3 != 3 {
		t.Fatalf("前置订单编号应为 1、2、3，实际 %d、%d、%d", bid1, bid2, bid3)
	}

	// 题面前置口径：现金 968、现金占用 97、可用 871；持仓 A=2、B=1。
	if e.Cash() != 968 || e.ReservedCash() != 97 || e.AvailableCash() != 871 {
		t.Fatalf("前置资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("前置持仓错误: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Limit != 200 || st.Holding != 40 || st.BuyReserved != 116 || st.Total != 156 {
		t.Fatalf("前置金额快照错误: %+v", st)
	}

	// 前置订单形态：订单1、2 部分成交（各剩 3），订单3 待成交（剩 2）。
	if o, _ := e.Order(bid1); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 3 {
		t.Fatalf("订单1 前置应为部分成交、已成交2、剩余3: %+v", o)
	}
	if o, _ := e.Order(bid2); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 3 {
		t.Fatalf("订单2 前置应为部分成交、已成交1、剩余3: %+v", o)
	}
	if o, _ := e.Order(bid3); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("订单3 前置应为待成交、剩余2: %+v", o)
	}
	return e, bid1, bid2, bid3
}

// wantAmountRefs 是金额撤单记录应固化的 A、B 报价定位（按合约代码排列）。
func wantAmountRefs() []RiskQuoteRef {
	return []RiskQuoteRef{
		{Symbol: "A", Seq: 1, Moment: 10, Price: 10},
		{Symbol: "B", Seq: 1, Moment: 20, Price: 20},
	}
}

// TestAmountLowerLimitCancelsDescendingStopsAtLimit 覆盖主场景：
// 上限 200 -> 76，按订单编号从大到小整单撤销订单3、订单2 的剩余量，
// 合计降到 76 恰好不超限即停止，不碰订单1；两条撤销记录分别固化撤销前的
// 判断金额与 A、B 当时的报价定位。
func TestAmountLowerLimitCancelsDescendingStopsAtLimit(t *testing.T) {
	e, bid1, bid2, bid3 := lowerAmountSetup(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(76)
	if err != nil {
		t.Fatalf("下调到 76 不应报错: %v", err)
	}
	// 返回编号顺序必须与实际撤销顺序一致：先订单3，再订单2。
	if !reflect.DeepEqual(canceled, []int64{bid3, bid2}) {
		t.Fatalf("应按编号从大到小撤销订单 %d、%d，实际 %v", bid3, bid2, canceled)
	}

	// 资金：撤单只释放未成交部分的限价冻结（16+45=61），已成交金额不退；
	// 现金仍为 968，占用降为 36（只剩订单1 的 3×12），可用 932。
	if e.Cash() != 968 || e.ReservedCash() != 36 || e.AvailableCash() != 932 {
		t.Fatalf("下调后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓不受撤单影响：A=2、B=1。
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("撤单不得改变持仓: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Limit != 76 || st.Holding != 40 || st.BuyReserved != 36 || st.Total != 76 {
		t.Fatalf("下调后金额应为持仓40+买单36=合计76: %+v", st)
	}

	// 订单形态：订单3 整单撤销（取消 2）；订单2 撤销未成交 3、保留已成交 1；
	// 订单1 仍为部分成交、剩余 3（合计恰好 76，不能继续撤销第一张）。
	o3, _ := e.Order(bid3)
	if o3.Status != StatusCanceled || o3.Qty != 2 || o3.Filled != 0 || o3.Remaining() != 0 {
		t.Fatalf("订单3 应整单撤销 2 份: %+v", o3)
	}
	o2, _ := e.Order(bid2)
	if o2.Status != StatusCanceled || o2.Qty != 4 || o2.Filled != 1 || o2.Remaining() != 0 {
		t.Fatalf("订单2 应只撤销未成交 3 份并保留已成交 1 份: %+v", o2)
	}
	o1, _ := e.Order(bid1)
	if o1.Status != StatusPartial || o1.Qty != 5 || o1.Filled != 2 || o1.Remaining() != 3 {
		t.Fatalf("订单1 应保持部分成交、剩余 3，不得继续撤销: %+v", o1)
	}

	// 恰好新增两条撤销记录，顺序与实际撤销一致。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 2 {
		t.Fatalf("本次调整应只新增 2 条撤销记录，实际 %d 条: %+v", len(newRecs), newRecs)
	}
	r3 := newRecs[0]
	r2 := newRecs[1]
	if r3.Kind != RecordCanceled || r3.OrderID != bid3 || r3.Symbol != "A" || r3.Side != Buy {
		t.Fatalf("第一条应为订单3（A）的撤销记录: %+v", r3)
	}
	if r2.Kind != RecordCanceled || r2.OrderID != bid2 || r2.Symbol != "B" || r2.Side != Buy {
		t.Fatalf("第二条应为订单2（B）的撤销记录: %+v", r2)
	}

	// 撤销数量必须与该单实际剩余量一致：订单3 取消 2（已成交0），
	// 订单2 取消 3（已成交1 保留）——绝不能只取消一张订单的一部分凑上限。
	if r3.Qty != 2 || r3.Filled != 0 || r3.Remaining != 2 {
		t.Fatalf("订单3 记录应取消其全部剩余 2 份: %+v", r3)
	}
	if r2.Qty != 4 || r2.Filled != 1 || r2.Remaining != 3 {
		t.Fatalf("订单2 记录应取消其全部剩余 3 份并保留已成交 1 份: %+v", r2)
	}

	// 两条记录分别保存撤销前的判断金额：第一条 40/116/156，第二条 40/96/136，
	// 上限均为新值 76；原因说明账户金额超限。
	for _, c := range []struct {
		name    string
		r       Record
		holding int64
		resv    int64
		total   int64
	}{
		{"第一条(订单3)", r3, 40, 116, 156},
		{"第二条(订单2)", r2, 40, 96, 136},
	} {
		if !c.r.AmtEnabled || c.r.AmtLimit != 76 || c.r.AmtHolding != c.holding ||
			c.r.AmtBuyReserved != c.resv || c.r.AmtTotal != c.total || c.r.AmtApplyTotal != 0 {
			t.Fatalf("%s应固化撤销前金额 持仓%d/买单%d/合计%d、上限76: %+v",
				c.name, c.holding, c.resv, c.total, c.r)
		}
		if !strings.Contains(c.r.Reason, "账户总持仓金额上限") || !strings.Contains(c.r.Reason, "超限") {
			t.Fatalf("%s撤销原因应说明账户金额超限: %q", c.name, c.r.Reason)
		}
		if !reflect.DeepEqual(c.r.AmtQuoteRefs, wantAmountRefs()) {
			t.Fatalf("%s应保存 A、B 当时的序号、时刻和价格: %+v", c.name, c.r.AmtQuoteRefs)
		}
	}

	// 金额额度不是现金冻结额度：前置校验已钉死两套口径的差异——同样三张单，
	// 现金按限价冻结共 97（订单1 3×12+订单2 3×15+订单3 2×8），金额占用按
	// max(限价,报价) 共 116（订单2 按 B 报价 20 计 60、订单3 按 A 报价 10 计 20）；
	// 撤单后现金占用与金额占用也分别独立核算（本场景两者恰好同为 36）。
}

// TestAmountLowerLimitExactBoundaryCancelsOne 钉住直接边界：
// 相同初始状态把上限改为 136，只撤销订单3，合计恰好 136 便停止；
// 订单2、订单1 保持有效，只新增一条撤销记录。
func TestAmountLowerLimitExactBoundaryCancelsOne(t *testing.T) {
	e, bid1, bid2, bid3 := lowerAmountSetup(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(136)
	if err != nil {
		t.Fatalf("下调到 136 不应报错: %v", err)
	}
	if !reflect.DeepEqual(canceled, []int64{bid3}) {
		t.Fatalf("合计恰好 136 时应只撤销订单 %d，实际 %v", bid3, canceled)
	}

	// 现金仍为 968；只释放订单3 的限价冻结 2×8=16，占用 81，可用 887。
	if e.Cash() != 968 || e.ReservedCash() != 81 || e.AvailableCash() != 887 {
		t.Fatalf("边界场景资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("边界场景持仓不得改变: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	// 撤掉订单3 后：持仓 40 + 买单占用（订单1 36 + 订单2 60）= 136，恰好等于上限。
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Limit != 136 || st.Holding != 40 || st.BuyReserved != 96 || st.Total != 136 {
		t.Fatalf("边界场景金额应为持仓40+买单96=合计136: %+v", st)
	}

	if o, _ := e.Order(bid3); o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("订单3 应已整单撤销: %+v", o)
	}
	if o, _ := e.Order(bid2); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 3 {
		t.Fatalf("订单2 合计恰好不超限应保留部分成交、剩余3: %+v", o)
	}
	if o, _ := e.Order(bid1); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 3 {
		t.Fatalf("订单1 不应被边界撤销触及: %+v", o)
	}

	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 1 || newRecs[0].Kind != RecordCanceled || newRecs[0].OrderID != bid3 {
		t.Fatalf("边界场景应只新增订单3 的一条撤销记录: %+v", newRecs)
	}
	r := newRecs[0]
	if r.AmtLimit != 136 || r.AmtHolding != 40 || r.AmtBuyReserved != 116 || r.AmtTotal != 156 {
		t.Fatalf("撤销记录应固化撤销前金额 40/116/156 与新上限 136: %+v", r)
	}
	if r.Remaining != 2 || r.Filled != 0 {
		t.Fatalf("应取消订单3 全部剩余 2 份: %+v", r)
	}
	if !reflect.DeepEqual(r.AmtQuoteRefs, wantAmountRefs()) {
		t.Fatalf("应保存 A、B 当时的报价定位: %+v", r.AmtQuoteRefs)
	}
}

// TestAmountLowerLimitNegativeRejectsBeforeAnyCancel 钉住另一个直接边界：
// 把上限改为负数必须报错，原上限 200、订单与资金占用全部保留，绝不能先撤单再报错。
func TestAmountLowerLimitNegativeRejectsBeforeAnyCancel(t *testing.T) {
	e, bid1, bid2, bid3 := lowerAmountSetup(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetPositionAmountLimit(-1)
	if err == nil {
		t.Fatal("负上限必须报错")
	}
	if !strings.Contains(err.Error(), "负") {
		t.Fatalf("错误应说明上限不能为负: %v", err)
	}
	if canceled != nil {
		t.Fatalf("报错时不得返回任何撤销编号: %v", canceled)
	}

	// 原上限 200 保留。
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Limit != 200 || st.Holding != 40 || st.BuyReserved != 116 || st.Total != 156 {
		t.Fatalf("负上限报错不得改变金额状态: %+v", st)
	}
	// 资金、持仓与占用全部保留。
	if e.Cash() != 968 || e.ReservedCash() != 97 || e.AvailableCash() != 871 {
		t.Fatalf("负上限报错不得改变资金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("负上限报错不得改变持仓: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	// 三张订单原样保留，没有任何一张被先撤再报错。
	if o, _ := e.Order(bid1); o.Status != StatusPartial || o.Remaining() != 3 {
		t.Fatalf("订单1 必须保留: %+v", o)
	}
	if o, _ := e.Order(bid2); o.Status != StatusPartial || o.Remaining() != 3 {
		t.Fatalf("订单2 必须保留: %+v", o)
	}
	if o, _ := e.Order(bid3); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("订单3 必须保留，不得先撤单再报错: %+v", o)
	}
	// 报错不追加任何记录。
	if len(e.Records()) != recsBefore {
		t.Fatalf("负上限报错不得新增记录，实际新增 %d 条", len(e.Records())-recsBefore)
	}
}
