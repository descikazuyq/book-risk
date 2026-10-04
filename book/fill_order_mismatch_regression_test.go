package book

import (
	"strings"
	"testing"
)

// 本文件为“成交回报必须与订单的合约和买卖方向一致”这条现有规则补充回归保障：
// 订单由编号确定，回报中的合约或方向与这笔委托不一致时整笔拒绝——即使被错填的
// 合约本身已有报价、限额、持仓和有效订单且资金充足，也不能转而寻找该合约的订单
// 入账；方向不一致时也不能根据回报方向调整原订单。拒绝只新增一条说明具体不一致
// 原因的拒绝记录（保留本次提交的订单编号、成交编号、合约、方向、价格和数量），
// 现金、可用现金、买单现金占用、各合约持仓与可卖数量、目标订单与其他有效订单的
// 状态、累计成交量和剩余量都保持提交前的值；目标订单已有部分成交时，已入账部分
// 不回退、未成交部分的占用不释放（卖单尤其要保留可卖数量占用）。错误回报不占用
// 成交编号：随后用同一编号把合约和方向改回该订单的实际值且其他成交条件合法时，
// 按普通成交成功入账，先前的拒绝记录保留。

// assertMismatchRejectRecord 校验拒绝后恰好新增一条记录，且该记录原样保留本次
// 提交的订单编号、成交编号、合约、方向、价格与数量，原因说明具体不一致内容。
func assertMismatchRejectRecord(t *testing.T, e *Engine, recsBase int, rec Record, wantReason ...string) {
	t.Helper()
	recs := e.Records()
	if len(recs) != recsBase+1 {
		t.Fatalf("拒绝后应只新增一条记录: 基础 %d 条，现有 %d 条", recsBase, len(recs))
	}
	r := recs[recsBase]
	if r.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", r)
	}
	if r.OrderID != rec.OrderID || r.TradeID != rec.TradeID || r.Symbol != rec.Symbol ||
		r.Side != rec.Side || r.TradePrice != rec.TradePrice || r.Qty != rec.Qty {
		t.Fatalf("拒绝记录必须保留提交的订单/成交编号、合约、方向、价格和数量: got %+v, want %+v", r, rec)
	}
	for _, w := range wantReason {
		if !strings.Contains(r.Reason, w) {
			t.Fatalf("拒绝原因 %q 必须包含 %q", r.Reason, w)
		}
	}
}

// TestFillSymbolMismatchRejectedThenCorrectedBooksNormally 覆盖合约不一致：
// 订单属于合约 A，回报却写成已有报价、限额、持仓和有效买单的合约 B（B 资金
// 充足、回报价格数量对 B 的订单也合法），仍须整笔拒绝，不能转而寻找 B 的订单
// 入账。目标买单已有部分成交：已入账的 4 股不回退，未成交 6 股的现金占用不
// 释放。随后用同一成交编号把合约改回 A 后按普通成交入账：只扣 20 现金、新增
// 2 股持仓、释放这 2 股对应的限价占用，订单累计成交 6 股、剩余 4 股；先前
// 拒绝记录保留，B 的账务与订单不受影响。
func TestFillSymbolMismatchRejectedThenCorrectedBooksNormally(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)

	// A：买单总量 10、限价 10，先以价格 9 成交 4 份（部分成交状态）。
	idA := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: idA, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	// B：已有持仓 2（全部成交的买单）与一笔有效买单（待成交 1 股限价 20）。
	idB1 := mustBuy(t, e, "B", 2, 20)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: idB1, Symbol: "B", Side: Buy, Price: 20, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	idB2 := mustBuy(t, e, "B", 1, 20)

	// 前置状态：现金 924、占用 80、可用 844；A 持仓 4，B 持仓 2。
	if e.Cash() != 924 || e.ReservedCash() != 80 || e.AvailableCash() != 844 ||
		e.Position("A") != 4 || e.Position("B") != 2 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	recsBase := len(e.Records())

	// 合约不一致：订单是 A 的买单，回报却写成 B。价格 10、数量 2 对 B 的
	// 有效买单（限价 20）同样合法，资金也充足，仍必须拒绝。
	res, err := e.Fill(Trade{TradeID: 3, OrderID: idA, Symbol: "B", Side: Buy, Price: 10, Qty: 2})
	if err == nil {
		t.Fatal("回报合约与订单合约不一致必须拒绝")
	}
	if res != (FillResult{}) {
		t.Fatalf("拒绝必须返回零值成交结果: %+v", res)
	}
	if !strings.Contains(err.Error(), "合约") || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("错误必须说明合约不一致: %v", err)
	}
	assertMismatchRejectRecord(t, e, recsBase,
		Record{OrderID: idA, TradeID: 3, Symbol: "B", Side: Buy, TradePrice: 10, Qty: 2},
		"合约", "不一致", "A", "B")
	// 拒绝记录按提交时合约 B 固化当时的报价与限额快照。
	if r := e.Records()[recsBase]; !r.QuoteValid || r.QuotePrice != 20 ||
		!r.MaxPositionValid || r.MaxPosition != 100 {
		t.Fatalf("拒绝记录应固化合约 B 当时的报价与限额快照: %+v", r)
	}

	// 一切业务状态保持提交前的值：资金、两合约持仓与可卖数量、目标订单
	// 已入账的 4 股与未成交 6 股的占用、B 的有效订单都不能改变。
	if e.Cash() != 924 || e.ReservedCash() != 80 || e.AvailableCash() != 844 {
		t.Fatalf("拒绝不得改变资金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Position("B") != 2 ||
		e.Sellable("A") != 4 || e.Sellable("B") != 2 {
		t.Fatalf("拒绝不得改变持仓与可卖数量: posA=%d posB=%d sellableA=%d sellableB=%d",
			e.Position("A"), e.Position("B"), e.Sellable("A"), e.Sellable("B"))
	}
	if o, _ := e.Order(idA); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 ||
		o.Side != Buy || o.Symbol != "A" {
		t.Fatalf("目标订单已入账部分不得回退、剩余占用不得释放: %+v", o)
	}
	if o, _ := e.Order(idB1); o.Status != StatusFilled || o.Filled != 2 {
		t.Fatalf("B 已成交订单不得受影响: %+v", o)
	}
	if o, _ := e.Order(idB2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 1 {
		t.Fatalf("不得转而寻找 B 的有效订单入账: %+v", o)
	}

	// 错误回报不占用成交编号：同一编号把合约改回 A，其他条件合法，按普通
	// 成交入账——只扣 2×10=20 现金，新增 2 股持仓，释放 2 股限价占用。
	res, err = e.Fill(Trade{TradeID: 3, OrderID: idA, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err != nil {
		t.Fatalf("修正后的合法成交必须成功: %v", err)
	}
	if res.TradeID != 3 || res.OrderID != idA || res.Price != 10 || res.Qty != 2 ||
		res.Filled != 6 || res.Remaining != 4 || res.Status != StatusPartial {
		t.Fatalf("修正后成交结果错误: %+v", res)
	}
	if e.Cash() != 904 || e.ReservedCash() != 60 || e.AvailableCash() != 844 {
		t.Fatalf("修正成交后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 6 || e.Sellable("A") != 6 {
		t.Fatalf("修正成交后 A 持仓应为 6: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(idA); o.Status != StatusPartial || o.Filled != 6 || o.Remaining() != 4 {
		t.Fatalf("订单应累计成交 6 股、剩余 4 股: %+v", o)
	}
	// B 的账务与订单全程不受影响。
	if e.Position("B") != 2 || e.Sellable("B") != 2 {
		t.Fatalf("B 持仓不得受影响: pos=%d sellable=%d", e.Position("B"), e.Sellable("B"))
	}
	if o, _ := e.Order(idB2); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("B 的有效订单不得受影响: %+v", o)
	}

	// 先前拒绝记录保留，其后只追加一条成交记录。
	recs := e.Records()
	if len(recs) != recsBase+2 {
		t.Fatalf("修正成交后应只有拒绝+成交两条新记录: 基础 %d，现有 %d", recsBase, len(recs))
	}
	if recs[recsBase].Kind != RecordRejected || recs[recsBase+1].Kind != RecordFilled ||
		recs[recsBase+1].TradeID != 3 || recs[recsBase+1].Symbol != "A" {
		t.Fatalf("拒绝记录必须保留且其后为合约 A 的成交记录: %+v", recs[recsBase:])
	}

	// 已入账成交的幂等重放返回原结果，不新增记录、不再次记账。
	res2, err := e.Fill(Trade{TradeID: 3, OrderID: idA, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err != nil || res2 != res {
		t.Fatalf("幂等重放必须返回第一次结果: res=%+v err=%v", res2, err)
	}
	if len(e.Records()) != recsBase+2 || e.Cash() != 904 || e.Position("A") != 6 {
		t.Fatal("幂等重放不得新增记录或再次记账")
	}
}

// TestFillSideMismatchRejectedThenCorrectedBooksNormally 覆盖方向不一致：
// 部分成交的买单收到卖出回报、待成交的卖单收到买入回报，都必须整笔拒绝，
// 不能根据回报方向调整原订单。卖单发生不一致时尤其要保留原有的可卖数量
// 占用。随后用同一成交编号改回订单实际方向后按普通成交入账：买单继续扣
// 现金、增持仓、释放限价占用；卖单按实际所得增加现金、减少持仓并释放
// 相应可卖占用。
func TestFillSideMismatchRejectedThenCorrectedBooksNormally(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	// 买单总量 10、限价 10，先成交 6 股（部分成交）；再以持仓提交卖单 4 股
	// 限价 8，占用全部 4 股可卖数量中的 4 股，剩余可卖 2。
	idBuy := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: idBuy, Symbol: "A", Side: Buy, Price: 10, Qty: 6}); err != nil {
		t.Fatal(err)
	}
	idSell := mustSell(t, e, "A", 4, 8)

	// 前置状态：现金 940、买单占用 40；持仓 6、可卖 2（卖单占用 4）。
	if e.Cash() != 940 || e.ReservedCash() != 40 || e.Position("A") != 6 || e.Sellable("A") != 2 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}
	recsBase := len(e.Records())

	// 方向不一致之一：买单收到卖出回报。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: idBuy, Symbol: "A", Side: Sell, Price: 10, Qty: 2})
	if err == nil {
		t.Fatal("买单收到卖出回报必须拒绝")
	}
	if res != (FillResult{}) {
		t.Fatalf("拒绝必须返回零值成交结果: %+v", res)
	}
	if !strings.Contains(err.Error(), "方向") || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("错误必须说明方向不一致: %v", err)
	}
	assertMismatchRejectRecord(t, e, recsBase,
		Record{OrderID: idBuy, TradeID: 2, Symbol: "A", Side: Sell, TradePrice: 10, Qty: 2},
		"方向", "不一致", "卖出", "买入")

	// 方向不一致之二：卖单收到买入回报。
	res, err = e.Fill(Trade{TradeID: 3, OrderID: idSell, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err == nil {
		t.Fatal("卖单收到买入回报必须拒绝")
	}
	if res != (FillResult{}) {
		t.Fatalf("拒绝必须返回零值成交结果: %+v", res)
	}
	assertMismatchRejectRecord(t, e, recsBase+1,
		Record{OrderID: idSell, TradeID: 3, Symbol: "A", Side: Buy, TradePrice: 10, Qty: 2},
		"方向", "不一致", "买入", "卖出")

	// 两笔拒绝都不得改变任何业务状态：资金、持仓、可卖数量（卖单的 4 股
	// 可卖占用必须保留）、两订单的方向/状态/累计成交与剩余量保持提交前的值。
	if e.Cash() != 940 || e.ReservedCash() != 40 || e.AvailableCash() != 900 {
		t.Fatalf("拒绝不得改变资金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 6 || e.Sellable("A") != 2 {
		t.Fatalf("拒绝不得改变持仓或可卖占用: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(idBuy); o.Side != Buy || o.Status != StatusPartial ||
		o.Filled != 6 || o.Remaining() != 4 {
		t.Fatalf("买单不得被回报方向调整，已入账 6 股与剩余占用保留: %+v", o)
	}
	if o, _ := e.Order(idSell); o.Side != Sell || o.Status != StatusPending ||
		o.Filled != 0 || o.Remaining() != 4 {
		t.Fatalf("卖单不得被回报方向调整，可卖占用不得释放: %+v", o)
	}

	// 同一成交编号改回订单实际方向：买单再成交 2 股 @10，只扣 20 现金、
	// 新增 2 股持仓、释放 20 限价占用，订单累计成交 8 股、剩余 2 股。
	res, err = e.Fill(Trade{TradeID: 2, OrderID: idBuy, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err != nil {
		t.Fatalf("修正后的买单成交必须成功: %v", err)
	}
	if res.Filled != 8 || res.Remaining != 2 || res.Status != StatusPartial {
		t.Fatalf("买单修正成交结果错误: %+v", res)
	}
	if e.Cash() != 920 || e.ReservedCash() != 20 || e.Position("A") != 8 {
		t.Fatalf("买单修正成交后状态错误: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	// 卖单的可卖占用在买单成交前后保持不变。
	if e.Sellable("A") != 4 {
		t.Fatalf("卖单占用应保持 4 股（可卖=持仓8-占用4）: sellable=%d", e.Sellable("A"))
	}

	// 卖单修正后按实际所得入账：成交 2 股 @9，现金 +18，持仓 -2，释放 2 股可卖占用。
	res, err = e.Fill(Trade{TradeID: 3, OrderID: idSell, Symbol: "A", Side: Sell, Price: 9, Qty: 2})
	if err != nil {
		t.Fatalf("修正后的卖单成交必须成功: %v", err)
	}
	if res.Filled != 2 || res.Remaining != 2 || res.Status != StatusPartial {
		t.Fatalf("卖单修正成交结果错误: %+v", res)
	}
	if e.Cash() != 938 || e.Position("A") != 6 || e.Sellable("A") != 4 {
		t.Fatalf("卖单修正成交后状态错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(idSell); o.Side != Sell || o.Status != StatusPartial ||
		o.Filled != 2 || o.Remaining() != 2 {
		t.Fatalf("卖单应累计成交 2 股、剩余 2 股: %+v", o)
	}

	// 两条拒绝记录保留，其后为两笔修正成交记录。
	recs := e.Records()
	if len(recs) != recsBase+4 {
		t.Fatalf("应只有两条拒绝加两条成交记录: 基础 %d，现有 %d", recsBase, len(recs))
	}
	if recs[recsBase].Kind != RecordRejected || recs[recsBase+1].Kind != RecordRejected ||
		recs[recsBase+2].Kind != RecordFilled || recs[recsBase+3].Kind != RecordFilled {
		t.Fatalf("拒绝记录必须保留且其后为两笔成交记录: %+v", recs[recsBase:])
	}
}
