package book

import (
	"strings"
	"testing"
)

// 本文件为“成交回报必须与订单编号所确定的委托一致”补充回归保障：
// 订单由编号确定，回报中的合约与买卖方向都必须和该笔委托一致才允许入账。
// 合约写错（订单属于 A，回报写成另一个已有报价、限额、持仓与有效订单的 B）
// 或方向写反（买单收到卖出回报、卖单收到买入回报）都必须整笔拒绝：返回错误
// 与零值成交结果，只新增一条说明具体不一致原因的拒绝记录（保留本次提交的
// 订单编号、成交编号、合约、方向、价格、数量），资金、持仓、可卖数量与所有
// 订单的状态、累计成交量、剩余量及各项占用都保持提交前的值；绝不能转而寻找
// 所填合约的订单入账，也不能按回报方向调整原订单。被拒绝的成交不占用编号，
// 把合约和方向改回该订单的实际值后，同一编号可按普通成交成功入账。

// assertFillMismatchRejected 校验一次不一致回报被拒绝后的全部不变量：
// 返回错误与零值结果、错误与记录都说明具体不一致原因、只新增一条拒绝记录，
// 且记录完整保留本次提交的订单编号、成交编号、合约、方向、价格与数量。
func assertFillMismatchRejected(t *testing.T, e *Engine, tr Trade, recsBefore int, wantReason ...string) {
	t.Helper()
	res, err := e.Fill(tr)
	if err == nil {
		t.Fatalf("不一致的成交 %+v 必须报错", tr)
	}
	for _, s := range wantReason {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("错误 %q 应包含 %q", err.Error(), s)
		}
	}
	if res != (FillResult{}) {
		t.Fatalf("不一致拒绝必须返回零值成交结果，实际 %+v", res)
	}
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("不一致拒绝只能新增一条记录，实际新增 %d 条", len(recs)-recsBefore)
	}
	rec := recs[len(recs)-1]
	if rec.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rec)
	}
	if rec.OrderID != tr.OrderID || rec.TradeID != tr.TradeID || rec.Symbol != tr.Symbol ||
		rec.Side != tr.Side || rec.TradePrice != tr.Price || rec.Qty != tr.Qty {
		t.Fatalf("拒绝记录必须保留本次提交的订单/成交编号、合约、方向、价格、数量: %+v", rec)
	}
	for _, s := range wantReason {
		if !strings.Contains(rec.Reason, s) {
			t.Fatalf("拒绝原因 %q 应包含 %q", rec.Reason, s)
		}
	}
}

// TestFillSymbolMismatchRejectedThenCorrected 覆盖合约不一致：目标订单属于 A，
// 已有部分成交；B 是另一个已有报价、限额、持仓与有效订单的合约且资金充足。
// 把回报合约写成 B 必须拒绝，不能转而寻找 B 的订单入账；随后用同一成交编号
// 改回 A 后按普通成交入账：只扣除本次成交金额、新增对应持仓、释放对应限价
// 占用，先前拒绝记录保留，B 合约与其他订单的账务不受影响。
func TestFillSymbolMismatchRejectedThenCorrected(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 11, Price: 5}, 1)

	// B 已有持仓 10 与一笔有效卖单（占用可卖 4），资金充足。
	bBuy := mustBuy(t, e, "B", 10, 5)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bBuy, Symbol: "B", Side: Buy, Price: 5, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	bSell := mustSell(t, e, "B", 4, 5)

	// 目标买单：A 总量 10、限价 10；另有一笔有效买单用于验证不受影响。
	aBuy := mustBuy(t, e, "A", 10, 10)
	aBuy2 := mustBuy(t, e, "A", 2, 10)

	// 先以价格 9 成交 4 份：现金 1000-50-36=914，占用 120-40=80，持仓 A=4。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: aBuy, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 914 || e.ReservedCash() != 80 || e.AvailableCash() != 834 {
		t.Fatalf("前置资金状态错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Sellable("A") != 4 || e.Position("B") != 10 || e.Sellable("B") != 6 {
		t.Fatalf("前置持仓状态错误: posA=%d sellableA=%d posB=%d sellableB=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"), e.Sellable("B"))
	}

	// 订单属于 A，回报却写成 B：即使 B 有有效订单且资金充足也必须拒绝，
	// 不能转而寻找 B 的订单入账。
	recsBefore := len(e.Records())
	assertFillMismatchRejected(t, e,
		Trade{TradeID: 3, OrderID: aBuy, Symbol: "B", Side: Buy, Price: 10, Qty: 2},
		recsBefore, "合约", "不一致", "B", "A")

	// 全部资金、持仓与订单状态保持提交前的值。
	if e.Cash() != 914 || e.ReservedCash() != 80 || e.AvailableCash() != 834 {
		t.Fatalf("合约不一致拒绝不得改变资金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Sellable("A") != 4 || e.Position("B") != 10 || e.Sellable("B") != 6 {
		t.Fatalf("合约不一致拒绝不得改变两个合约的持仓与可卖: posA=%d sellableA=%d posB=%d sellableB=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"), e.Sellable("B"))
	}
	if o, _ := e.Order(aBuy); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("目标订单已入账部分不得回退、剩余占用不得释放: %+v", o)
	}
	if o, _ := e.Order(aBuy2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("其他有效买单不得受影响: %+v", o)
	}
	if o, _ := e.Order(bSell); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 4 {
		t.Fatalf("B 合约的有效订单不得被转而入账: %+v", o)
	}

	// 同一编号把合约改回 A、其余条件合法：按普通成交成功入账。
	res, err := e.Fill(Trade{TradeID: 3, OrderID: aBuy, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err != nil {
		t.Fatalf("被拒绝的成交编号改回合约会应可复用: %v", err)
	}
	if res.TradeID != 3 || res.OrderID != aBuy || res.Price != 10 || res.Qty != 2 ||
		res.Filled != 6 || res.Remaining != 4 || res.Status != StatusPartial {
		t.Fatalf("修正后成交结果错误: %+v", res)
	}
	// 只扣除 2×10=20 现金，新增 2 份持仓，释放这 2 份对应的限价占用。
	if e.Cash() != 894 || e.ReservedCash() != 60 || e.AvailableCash() != 834 {
		t.Fatalf("修正后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 6 || e.Sellable("A") != 6 {
		t.Fatalf("修正后持仓错误: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(aBuy); o.Status != StatusPartial || o.Filled != 6 || o.Remaining() != 4 {
		t.Fatalf("订单应累计成交 6、剩余 4: %+v", o)
	}
	// B 合约与其他订单的账务不受影响。
	if e.Position("B") != 10 || e.Sellable("B") != 6 {
		t.Fatalf("B 合约账务不得受影响: pos=%d sellable=%d", e.Position("B"), e.Sellable("B"))
	}
	if o, _ := e.Order(aBuy2); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("其他有效买单不得受影响: %+v", o)
	}
	if o, _ := e.Order(bSell); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 4 {
		t.Fatalf("B 合约有效订单不得受影响: %+v", o)
	}

	// 先前的拒绝记录保留；修正后只新增一条成交记录。
	recs := e.Records()
	if len(recs) != recsBefore+2 {
		t.Fatalf("修正成交后应只比拒绝前多两条记录（拒绝+成交），实际 %d 条", len(recs)-recsBefore)
	}
	rej, fill := recs[recsBefore], recs[recsBefore+1]
	if rej.Kind != RecordRejected || rej.TradeID != 3 || rej.Symbol != "B" {
		t.Fatalf("先前拒绝记录必须保留: %+v", rej)
	}
	if fill.Kind != RecordFilled || fill.TradeID != 3 || fill.Symbol != "A" || fill.Side != Buy ||
		fill.TradePrice != 10 || fill.Qty != 2 || fill.Filled != 6 || fill.Remaining != 4 {
		t.Fatalf("修正后的成交记录错误: %+v", fill)
	}

	// 已入账的成交幂等重放：返回原结果，不再新增记录或重复记账。
	again, err := e.Fill(Trade{TradeID: 3, OrderID: aBuy, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err != nil || again != res {
		t.Fatalf("幂等重放应返回原结果: %+v, %v", again, err)
	}
	if len(e.Records()) != len(recs) || e.Cash() != 894 || e.Position("A") != 6 {
		t.Fatalf("幂等重放不得新增记录或重复记账: recs=%d cash=%d pos=%d",
			len(e.Records()), e.Cash(), e.Position("A"))
	}
}

// TestFillSideMismatchRejectedThenCorrected 覆盖方向不一致：买单收到卖出回报、
// 卖单收到买入回报都必须拒绝，不能根据回报方向调整原订单；同一编号改回实际
// 方向后按普通成交入账。
func TestFillSideMismatchRejectedThenCorrected(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	b0 := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b0, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	// 目标买单（待成交）与目标卖单（占用 6 可卖）。
	aBuy := mustBuy(t, e, "A", 5, 10)
	aSell := mustSell(t, e, "A", 6, 9)

	if e.Cash() != 900 || e.ReservedCash() != 50 || e.Position("A") != 10 || e.Sellable("A") != 4 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}

	// 买单收到卖出回报：拒绝，不能按回报方向把买单当卖单处理。
	recsBefore := len(e.Records())
	assertFillMismatchRejected(t, e,
		Trade{TradeID: 2, OrderID: aBuy, Symbol: "A", Side: Sell, Price: 10, Qty: 2},
		recsBefore, "方向", "不一致", "卖出", "买入")
	// 卖单收到买入回报：同样拒绝。
	assertFillMismatchRejected(t, e,
		Trade{TradeID: 3, OrderID: aSell, Symbol: "A", Side: Buy, Price: 10, Qty: 2},
		recsBefore+1, "方向", "不一致", "买入", "卖出")

	// 资金、持仓、可卖与两笔订单的方向、状态、剩余量全部保持提交前的值。
	if e.Cash() != 900 || e.ReservedCash() != 50 || e.AvailableCash() != 850 {
		t.Fatalf("方向不一致拒绝不得改变资金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 10 || e.Sellable("A") != 4 {
		t.Fatalf("方向不一致拒绝不得改变持仓与可卖占用: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(aBuy); o.Side != Buy || o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 5 {
		t.Fatalf("买单不得按回报方向调整: %+v", o)
	}
	if o, _ := e.Order(aSell); o.Side != Sell || o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 6 {
		t.Fatalf("卖单不得按回报方向调整: %+v", o)
	}

	// 同一编号改回实际方向后按普通成交入账。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: aBuy, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err != nil {
		t.Fatalf("买单修正方向后同编号成交应成功: %v", err)
	}
	if res.Filled != 2 || res.Remaining != 3 || res.Status != StatusPartial {
		t.Fatalf("买单修正后成交结果错误: %+v", res)
	}
	if e.Cash() != 880 || e.ReservedCash() != 30 || e.Position("A") != 12 {
		t.Fatalf("买单修正后账务错误: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}

	res, err = e.Fill(Trade{TradeID: 3, OrderID: aSell, Symbol: "A", Side: Sell, Price: 9, Qty: 2})
	if err != nil {
		t.Fatalf("卖单修正方向后同编号成交应成功: %v", err)
	}
	if res.Filled != 2 || res.Remaining != 4 || res.Status != StatusPartial {
		t.Fatalf("卖单修正后成交结果错误: %+v", res)
	}
	// 卖出按实际所得 2×9=18 增加现金、减少持仓并释放相应可卖占用。
	if e.Cash() != 898 || e.Position("A") != 10 || e.Sellable("A") != 6 {
		t.Fatalf("卖单修正后账务错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}

	// 两条方向不一致的拒绝记录都保留。
	recs := e.Records()
	for i, tr := range []Trade{
		{TradeID: 2, OrderID: aBuy, Symbol: "A", Side: Sell, Price: 10, Qty: 2},
		{TradeID: 3, OrderID: aSell, Symbol: "A", Side: Buy, Price: 10, Qty: 2},
	} {
		rec := recs[recsBefore+i]
		if rec.Kind != RecordRejected || rec.TradeID != tr.TradeID || rec.Side != tr.Side {
			t.Fatalf("第 %d 条方向不一致拒绝记录必须保留: %+v", i+1, rec)
		}
	}
}

// TestFillSellMismatchKeepsSellableReservation 覆盖已有部分成交的卖单发生
// 合约不一致：已入账部分不能回退，尚未成交部分的可卖占用尤其要保留；
// 修正后同一编号按实际所得增加现金、减少持仓并释放相应可卖占用。
func TestFillSellMismatchKeepsSellableReservation(t *testing.T) {
	e, _ := NewEngine(500)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 11, Price: 7}, 1)

	b0 := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b0, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 8, 8) // 占用 8 可卖，可卖余 2
	// 先部分成交 3 股 @9：现金 400+27=427，持仓 7，卖单占用余 5。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 9, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 427 || e.Position("A") != 7 || e.Sellable("A") != 2 {
		t.Fatalf("前置状态错误: cash=%d pos=%d sellable=%d", e.Cash(), e.Position("A"), e.Sellable("A"))
	}

	// 部分成交的卖单收到合约 B 的回报：拒绝；B 也有报价、限额与充足现金。
	recsBefore := len(e.Records())
	assertFillMismatchRejected(t, e,
		Trade{TradeID: 3, OrderID: sid, Symbol: "B", Side: Sell, Price: 9, Qty: 2},
		recsBefore, "合约", "不一致", "B", "A")

	// 已入账的 3 股不回退，未成交 5 股的可卖占用不释放。
	if e.Cash() != 427 || e.Position("A") != 7 || e.Sellable("A") != 2 {
		t.Fatalf("卖单不一致拒绝不得改变资金、持仓与可卖占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Status != StatusPartial || o.Filled != 3 || o.Remaining() != 5 {
		t.Fatalf("卖单累计成交与剩余占用必须保留: %+v", o)
	}
	if e.Position("B") != 0 || e.Sellable("B") != 0 {
		t.Fatalf("B 合约不得被转而入账: pos=%d sellable=%d", e.Position("B"), e.Sellable("B"))
	}

	// 同一编号改回 A 后按普通成交入账：现金 +2×9=18，持仓 -2，释放 2 可卖占用。
	res, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: 9, Qty: 2})
	if err != nil {
		t.Fatalf("卖单修正合约后同编号成交应成功: %v", err)
	}
	if res.Filled != 5 || res.Remaining != 3 || res.Status != StatusPartial {
		t.Fatalf("卖单修正后成交结果错误: %+v", res)
	}
	if e.Cash() != 445 || e.Position("A") != 5 || e.Sellable("A") != 2 {
		t.Fatalf("卖单修正后应按实际所得入账并释放占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Filled != 5 || o.Remaining() != 3 {
		t.Fatalf("卖单应累计成交 5、剩余 3: %+v", o)
	}

	// 先前拒绝记录保留，且只新增了一条拒绝与一条成交。
	recs := e.Records()
	if len(recs) != recsBefore+2 {
		t.Fatalf("修正成交后应只比拒绝前多两条记录，实际 %d 条", len(recs)-recsBefore)
	}
	if recs[recsBefore].Kind != RecordRejected || recs[recsBefore].TradeID != 3 ||
		recs[recsBefore].Symbol != "B" {
		t.Fatalf("先前拒绝记录必须保留: %+v", recs[recsBefore])
	}
}
