package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“下调持仓限额时选中部分成交后又修改过的买单”补充回归保障：
// 撤单只释放取消部分按当前限价占用的现金，取消数量按修改后的总量减去已成交量
// 计算；修改不改变订单最初接受的位置，不能当作重新下单。两种超限原因
// （持仓本身超限 / 加上有效买单剩余量才超限）共用同一套选择规则，
// 本文件同时钉住这两条路径上修改订单的口径。
//
// 题述例：买单已成交 3 份，随后改为总量 7、限价 12，被限额调整选中时应取消
// 4 份、按 12 释放 48 现金占用，保留 3 份持仓与既有成交结果。

// modifiedCancelSetup 构造共用前置：
//   - 初始现金 1000；A、B 最大持仓量均为 100。
//   - 报价 A 序号1、时刻10、价格10；B 序号1、时刻20、价格5。
//   - A 买单 10@10（bid）成交 3 份 @9：现金 973、A 持仓 3。
//   - bid 修改为总量 7、限价 12：剩余 4 份只占 4×12=48 现金。
//   - 另有 B 买单 4@5（bidB，接受得更晚）与 A 卖单 2@8（sellA）。
func modifiedCancelSetup(t *testing.T) (e *Engine, bid, bidB, sellA int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 5}, 1)

	bid = mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 9, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	mustModify(t, e, bid, 7, 12) // 已成交 3，新剩余 4，按 12 占用 48
	bidB = mustBuy(t, e, "B", 4, 5)
	sellA = mustSell(t, e, "A", 2, 8)

	if e.Cash() != 973 || e.ReservedCash() != 68 || e.AvailableCash() != 905 {
		t.Fatalf("前置资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 3 || e.Sellable("A") != 1 {
		t.Fatalf("前置持仓错误: A.pos=%d A.sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(bid); o.Qty != 7 || o.Limit != 12 || o.Filled != 3 || o.Remaining() != 4 {
		t.Fatalf("修改订单前置状态错误: %+v", o)
	}
	return e, bid, bidB, sellA
}

// TestSetMaxPositionModifiedBuyCancelUsesNewQtyAndLimit 覆盖题述例的普通超限路径：
// 恰好等于新限额时保留较早订单；再降 1 即选中这张修改过的订单，取消 4 份、
// 按当前限价 12 释放 48，已成交 3 份与既有成交金额不动。
func TestSetMaxPositionModifiedBuyCancelUsesNewQtyAndLimit(t *testing.T) {
	e, bid, bidB, sellA := modifiedCancelSetup(t)

	// 持仓 3 + 有效买单剩余 4 = 7：新限额恰好 7，不撤单、不新增记录。
	recsBefore := len(e.Records())
	canceled, err := e.SetMaxPosition("A", 7)
	if err != nil {
		t.Fatalf("下调 A 限额到 7 不应报错: %v", err)
	}
	if len(canceled) != 0 {
		t.Fatalf("恰好等于新限额时应保留订单，实际撤销 %v", canceled)
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Remaining() != 4 {
		t.Fatalf("恰好等于限额时修改订单应保持有效: %+v", o)
	}
	if len(e.Records()) != recsBefore {
		t.Fatal("恰好用满额度的合法调整不得新增撤销记录")
	}

	// 降到 6：3+4=7 > 6，选中 bid（A 唯一有效买单），取消修改后的全部剩余 4 份。
	canceled, err = e.SetMaxPosition("A", 6)
	if err != nil {
		t.Fatalf("下调 A 限额到 6 不应报错: %v", err)
	}
	if len(canceled) != 1 || canceled[0] != bid {
		t.Fatalf("应只撤销修改过的 A 买单 %d，实际 %v", bid, canceled)
	}

	// 资金：按当前限价 12 释放 4 份共 48；已成交 3 份的 27 不退回，
	// 现金余额仍为 973；B 买单的 20 占用原样保留。
	if e.Cash() != 973 || e.ReservedCash() != 20 || e.AvailableCash() != 953 {
		t.Fatalf("撤单资金口径错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓保留 3 份，卖单占用不变。
	if e.Position("A") != 3 || e.Sellable("A") != 1 {
		t.Fatalf("撤单不得改动持仓与卖单占用: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}

	o, _ := e.Order(bid)
	if o.Status != StatusCanceled || o.Qty != 7 || o.Limit != 12 || o.Filled != 3 || o.Remaining() != 0 {
		t.Fatalf("修改订单应保留新总量 7 与已成交 3、撤销剩余 4: %+v", o)
	}
	if !strings.Contains(o.Reason, "6") {
		t.Fatalf("撤销原因应说明限额下调至 6: %q", o.Reason)
	}

	// 撤销记录：总量取修改后的 7、已成交 3、本次取消 4（不是原委托量 10），
	// 报价与新限额 6 快照齐备。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 1 {
		t.Fatalf("应只新增 1 条撤销记录，实际 %d 条: %+v", len(newRecs), newRecs)
	}
	cr := newRecs[0]
	if cr.Kind != RecordCanceled || cr.OrderID != bid || cr.Symbol != "A" || cr.Side != Buy {
		t.Fatalf("应为修改订单的撤销记录: %+v", cr)
	}
	if cr.Limit != 12 || cr.Qty != 7 || cr.Filled != 3 || cr.Remaining != 4 {
		t.Fatalf("撤销记录应区分新总量 7、已成交 3 与本次取消 4: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 1 || cr.QuoteMoment != 10 || cr.QuotePrice != 10 {
		t.Fatalf("撤销记录应携带调整当时的 A 报价: %+v", cr)
	}
	if !cr.MaxPositionValid || cr.MaxPosition != 6 {
		t.Fatalf("撤销记录应携带新限额 6 快照: %+v", cr)
	}

	// 未被选中的 B 买单与 A 卖单原样保留。
	if oB, _ := e.Order(bidB); oB.Status != StatusPending || oB.Remaining() != 4 {
		t.Fatalf("其他合约买单不得被选中: %+v", oB)
	}
	if oSell, _ := e.Order(sellA); oSell.Status != StatusPending || oSell.Remaining() != 2 {
		t.Fatalf("该合约卖单不得被选中: %+v", oSell)
	}

	// 再降到 2：持仓 3 本身已超限，但已无 A 有效买单——返回空列表、不新增记录，
	// 持仓继续保留；两种超限原因走同一选择规则，都选不出已撤销的订单。
	recsBefore = len(e.Records())
	canceled, err = e.SetMaxPosition("A", 2)
	if err != nil {
		t.Fatalf("下调 A 限额到 2 不应报错: %v", err)
	}
	if len(canceled) != 0 {
		t.Fatalf("已无有效买单时不应再撤单，实际 %v", canceled)
	}
	if len(e.Records()) != recsBefore {
		t.Fatal("无有效买单可撤时不得新增撤销记录")
	}
	if e.Position("A") != 3 || e.ReservedCash() != 20 {
		t.Fatalf("持仓超限不自动平仓、B 买单占用保留: pos=%d reserved=%d",
			e.Position("A"), e.ReservedCash())
	}
	if o, _ := e.Order(bid); o.Status != StatusCanceled || o.Filled != 3 {
		t.Fatalf("已撤销订单不应被重复处理: %+v", o)
	}
}

// TestSetMaxPositionModifiedBuyKeepsOriginalAcceptOrder 钉住“修改不是重新下单”：
// 较早接受、随后修改过的 bid 仍排在后接受的 A 买单之前，撤单自最后接受的订单起。
func TestSetMaxPositionModifiedBuyKeepsOriginalAcceptOrder(t *testing.T) {
	e, bid, _, _ := modifiedCancelSetup(t)

	// 再接受一张 A 买单 2@10（later）：持仓 3 + bid 剩余 4 + later 2 = 9。
	later := mustBuy(t, e, "A", 2, 10)
	if e.ReservedCash() != 48+20+20 { // bid 48 + later 20 + B 单 20
		t.Fatalf("新增买单后占用错误: reserved=%d", e.ReservedCash())
	}

	// 限额降到 6：先撤最后接受的 later（释放 2×10=20），3+4=7 仍超；
	// 再撤较早接受但修改过的 bid（取消 4、释放 4×12=48），3 ≤ 6 停止。
	canceled, err := e.SetMaxPosition("A", 6)
	if err != nil {
		t.Fatalf("下调 A 限额到 6 不应报错: %v", err)
	}
	if want := []int64{later, bid}; !reflect.DeepEqual(canceled, want) {
		t.Fatalf("应按最初接受次序撤销 %v，实际 %v", want, canceled)
	}
	if e.Cash() != 973 { // 既有成交 3×9=27 不退回
		t.Fatalf("撤单不得触动已成交金额: cash=%d", e.Cash())
	}
	if e.ReservedCash() != 20 { // 只剩 B 买单 4×5=20
		t.Fatalf("两单撤销后应只剩 B 买单占用 20: reserved=%d", e.ReservedCash())
	}
	if e.Position("A") != 3 {
		t.Fatalf("持仓应保留 3: pos=%d", e.Position("A"))
	}

	// 返回的编号顺序与新增撤销记录顺序一致，每张订单恰好一条记录。
	var recOrder []int64
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled && (r.OrderID == bid || r.OrderID == later) {
			recOrder = append(recOrder, r.OrderID)
		}
	}
	if want := []int64{later, bid}; !reflect.DeepEqual(recOrder, want) {
		t.Fatalf("撤销记录顺序应为 %v，实际 %v", want, recOrder)
	}
	// bid 的记录仍按修改后的数量口径：总量 7、已成交 3、取消 4、限价 12。
	var bidRec *Record
	recs := e.Records()
	for i := range recs {
		if recs[i].Kind == RecordCanceled && recs[i].OrderID == bid {
			bidRec = &recs[i]
		}
	}
	if bidRec == nil {
		t.Fatal("应存在 bid 的撤销记录")
	}
	if bidRec.Limit != 12 || bidRec.Qty != 7 || bidRec.Filled != 3 || bidRec.Remaining != 4 {
		t.Fatalf("bid 撤销记录数量口径错误: %+v", *bidRec)
	}
}

// TestSetMaxPositionInvalidArgKeepsSetting 覆盖空合约与负限额：报错且保留原设置，
// 不撤单、不新增撤销记录。
func TestSetMaxPositionInvalidArgKeepsSetting(t *testing.T) {
	e, bid, _, _ := modifiedCancelSetup(t)
	recsBefore := len(e.Records())

	if _, err := e.SetMaxPosition("", 6); err == nil {
		t.Fatal("空合约代码必须报错")
	}
	if _, err := e.SetMaxPosition("A", -1); err == nil {
		t.Fatal("负持仓限额必须报错")
	}
	if max, ok := e.MaxPosition("A"); !ok || max != 100 {
		t.Fatalf("非法参数必须保留原设置 100: max=%d ok=%v", max, ok)
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Remaining() != 4 {
		t.Fatalf("非法参数不得撤单或改动订单: %+v", o)
	}
	if e.Cash() != 973 || e.ReservedCash() != 68 {
		t.Fatalf("非法参数不得改变资金占用: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}
	if len(e.Records()) != recsBefore {
		t.Fatal("非法参数不得新增任何记录")
	}
}
