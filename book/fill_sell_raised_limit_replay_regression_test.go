package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“部分成交的卖单调高限价后，已入账成交的幂等重放与尚未入账新成交的
// 拒绝口径”补充回归保障。给已经部分成交的卖单调高限价后，先前入账的成交价格
// 可能低于新限价，但这笔历史成交仍然有效：同一成交编号、完全相同内容的再次提交
// 必须原样返回第一次的成交结果（第一次入账时的累计成交量、剩余量与部分成交
// 状态），不能因价格低于新限价而拒绝，也不能把订单当前剩余量回填到历史结果；
// 现金、持仓、可卖数量、两张订单与记录数量均保持重放前的值，订单查询继续显示
// 修改后的总量与限价。
//
// 尚未入账的新成交则按处理时的有效参数判断：价格低于新限价时整笔拒绝，返回错误
// 与零值成交结果，只追加一条说明“成交价格低于当前限价”的拒绝记录并保留本次回报
// 的订单编号、成交编号、合约、方向、价格与数量，不扣持仓、不加现金、不释放卖单
// 占用；被拒绝不占用成交编号，把价格修正到不低于新限价后沿用同一编号与数量应
// 正常入账，先前的拒绝记录保留，新成交只追加对应的成交记录。整个过程不改变
// Modify 修改订单与 Fill 逐笔结算的公开行为。

// sellRaisedLimitLedger 是关键操作前后必须保持一致的账户快照。
type sellRaisedLimitLedger struct {
	cash     int64
	position int64
	sellable int64
	target   Order
	other    Order
	recs     int
}

func snapshotSellRaisedLimitLedger(t *testing.T, e *Engine, target, other int64) sellRaisedLimitLedger {
	t.Helper()
	to, ok1 := e.Order(target)
	oo, ok2 := e.Order(other)
	if !ok1 || !ok2 {
		t.Fatalf("订单快照缺失: target=%v other=%v", ok1, ok2)
	}
	return sellRaisedLimitLedger{
		cash:     e.Cash(),
		position: e.Position("A"),
		sellable: e.Sellable("A"),
		target:   to,
		other:    oo,
		recs:     len(e.Records()),
	}
}

// setupSellRaisedLimitReplay 准备题述场景（未开日内亏损保护、行情连续）：
// 现金 200，先按价格 10 买入 A 合约 10 份（现金 100、持仓 10）；随后接受总量 8、
// 限价 8 的目标卖单，再接受一张总量 2 的卖单占用余下可卖数量；目标卖单先以价格 9
// 成交 3 份（现金 127、持仓 7，目标订单累计成交 3、剩余 5，两张卖单占满可卖
// 数量）；最后把目标卖单改为总量 6、限价 10——已有 3 份成交不重新计价，现金与
// 持仓不变，目标订单剩余 3，可卖数量变为 2，另一张卖单仍保留自己的 2 份占用。
//
// 返回引擎、两张卖单编号与第一次成交（编号 2、价格 9、数量 3）的成交结果。
func setupSellRaisedLimitReplay(t *testing.T) (e *Engine, target, other int64, first FillResult) {
	t.Helper()
	e, _ = NewEngine(200)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	// 先买入同一合约 10 份，每份成交价 10：买单按 10×10 占用现金，成交后
	// 现金 200-100=100、持仓 10，买单全部成交、占用清零。
	buyID := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: buyID, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatalf("前置买入成交应成功: %v", err)
	}
	if e.Cash() != 100 || e.ReservedCash() != 0 || e.AvailableCash() != 100 || e.Position("A") != 10 {
		t.Fatalf("买入成交后账务错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 接受总量 8、限价 8 的目标卖单；再让另一张卖单占用余下 2 份可卖数量。
	target = mustSell(t, e, "A", 8, 8)
	other = mustSell(t, e, "A", 2, 8)
	if e.Sellable("A") != 0 {
		t.Fatalf("两张卖单应占满可卖数量，实际可卖 %d", e.Sellable("A"))
	}

	// 目标卖单先以价格 9 成交 3 份：现金 100+27=127，持仓 10-3=7，
	// 目标订单累计成交 3、剩余 5、部分成交。
	firstTrade := Trade{TradeID: 2, OrderID: target, Symbol: "A", Side: Sell, Price: 9, Qty: 3}
	first, err := e.Fill(firstTrade)
	if err != nil {
		t.Fatalf("目标卖单首次部分成交应成功: %v", err)
	}
	if first.TradeID != 2 || first.OrderID != target || first.Price != 9 || first.Qty != 3 ||
		first.Filled != 3 || first.Remaining != 5 || first.Status != StatusPartial {
		t.Fatalf("第一次成交结果错误: %+v", first)
	}
	if e.Cash() != 127 || e.Position("A") != 7 || e.Sellable("A") != 0 {
		t.Fatalf("首次卖出成交后账务错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(target); o.Qty != 8 || o.Limit != 8 || o.Filled != 3 ||
		o.Status != StatusPartial || o.Remaining() != 5 {
		t.Fatalf("目标订单首次成交后状态错误: %+v", o)
	}

	// 把目标卖单改为总量 6、限价 10：已有 3 份成交不重新计价，现金、持仓不变；
	// 未成交占用由 5 份替换为 3 份，另一张卖单的 2 份占用保留，可卖数量变为 2。
	mustModify(t, e, target, 6, 10)
	if e.Cash() != 127 || e.Position("A") != 7 || e.Sellable("A") != 2 {
		t.Fatalf("修改后账务错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	to, _ := e.Order(target)
	if to.Qty != 6 || to.Limit != 10 || to.Filled != 3 ||
		to.Status != StatusPartial || to.Remaining() != 3 {
		t.Fatalf("目标订单修改后应为总量 6、限价 10、累计成交 3、剩余 3: %+v", to)
	}
	if o, _ := e.Order(other); o.Qty != 2 || o.Limit != 8 || o.Filled != 0 ||
		o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("另一张卖单必须保留自己的 2 份占用且不受修改影响: %+v", o)
	}
	return e, target, other, first
}

// TestSellRaisedLimitHistoricalFillReplayRegression 覆盖题述核心：调高部分成交卖单的
// 限价后，以原成交编号及完全相同内容再次提交先前那笔价格 9、数量 3 的成交，仍须
// 成功返回第一次的成交结果（累计成交 3、剩余 5、部分成交）——不能因价格 9 低于
// 新限价 10 而拒绝，也不能把当前剩余 3 回填为历史结果的剩余量；账户现金、持仓、
// 可卖数量、两张订单与记录数量均保持重放前的值，订单查询继续显示修改后的总量 6
// 与限价 10。
func TestSellRaisedLimitHistoricalFillReplayRegression(t *testing.T) {
	e, target, other, first := setupSellRaisedLimitReplay(t)
	firstTrade := Trade{TradeID: 2, OrderID: target, Symbol: "A", Side: Sell, Price: 9, Qty: 3}

	before := snapshotSellRaisedLimitLedger(t, e, target, other)
	if before.recs != 6 {
		t.Fatalf("重放前应有 6 条记录（接受买单、成交买单、接受卖单×2、成交卖单、修改），实际 %d",
			before.recs)
	}
	recordsBefore := e.Records()

	// 再次提交先前那笔成交：原成交编号、完全相同的内容。即使价格 9 低于新限价 10，
	// 也必须走幂等重放，返回第一次的成交结果而不是按新限价拒绝。
	replay, err := e.Fill(firstTrade)
	if err != nil {
		t.Fatalf("已入账成交以相同内容重放不应报错（即使价格低于新限价）: %v", err)
	}
	if replay != first {
		t.Fatalf("重放必须原样返回第一次成交结果（累计成交 3、剩余 5、部分成交）:\n第一次 %+v\n重放   %+v",
			first, replay)
	}
	if replay.Filled != 3 || replay.Remaining != 5 || replay.Status != StatusPartial {
		t.Fatalf("重放结果必须保留第一次入账时点的累计 3、剩余 5、部分成交，不得回填当前剩余 3: %+v",
			replay)
	}

	// 现金、持仓、可卖数量、两张订单和记录数量均保持重放前的值。
	after := snapshotSellRaisedLimitLedger(t, e, target, other)
	if after != before {
		t.Fatalf("幂等重放不得改变任何账户状态:\n之前 %+v\n之后 %+v", before, after)
	}
	recs := e.Records()
	for i, want := range recordsBefore {
		if !reflect.DeepEqual(recs[i], want) {
			t.Fatalf("第 %d 条原有记录被重放改写:\nwant %+v\ngot  %+v", i, want, recs[i])
		}
	}

	// 订单查询继续显示修改后的总量 6 与限价 10，历史结果不影响订单现状。
	to, _ := e.Order(target)
	if to.Qty != 6 || to.Limit != 10 || to.Filled != 3 || to.Remaining() != 3 ||
		to.Status != StatusPartial {
		t.Fatalf("重放后订单查询仍须显示修改后的总量 6、限价 10、剩余 3: %+v", to)
	}
}

// TestSellRaisedLimitNewFillRejectedThenCorrectedRegression 覆盖题述后半段：用一个
// 尚未成功入账的成交编号提交价格 9（低于新限价 10）、数量 2 的新成交，应整笔
// 拒绝（错误与零值结果，只追加一条说明价格低于当前限价的拒绝记录，保留本次回报
// 全部字段），不扣持仓、不加现金、不释放卖单占用；把价格修正为 10 后沿用刚被
// 拒绝的编号与数量应正常入账：现金 147、持仓 5，目标订单累计成交 5、剩余 1、
// 仍为部分成交，可卖数量仍为 2，另一张卖单不受影响。先前的拒绝记录保留，新入账
// 只追加对应的成交记录。
func TestSellRaisedLimitNewFillRejectedThenCorrectedRegression(t *testing.T) {
	e, target, other, first := setupSellRaisedLimitReplay(t)

	// 先重放一次历史成交，确认它不会占用或影响随后的新成交（重放不留记录）。
	firstTrade := Trade{TradeID: 2, OrderID: target, Symbol: "A", Side: Sell, Price: 9, Qty: 3}
	if replay, err := e.Fill(firstTrade); err != nil || replay != first {
		t.Fatalf("前置：历史成交重放应返回第一次结果: replay=%+v err=%v", replay, err)
	}

	before := snapshotSellRaisedLimitLedger(t, e, target, other)
	if before.cash != 127 || before.position != 7 || before.sellable != 2 {
		t.Fatalf("前置账务应为现金 127、持仓 7、可卖 2: %+v", before)
	}

	// 用一个尚未成功入账的成交编号提交价格 9、数量 2 的新成交：9 < 新限价 10，
	// 必须整笔拒绝。
	rejected := Trade{TradeID: 3, OrderID: target, Symbol: "A", Side: Sell, Price: 9, Qty: 2}
	res, err := e.Fill(rejected)
	if err == nil {
		t.Fatal("成交价 9 低于当前限价 10 必须报错")
	}
	if !strings.Contains(err.Error(), "成交 3 被拒绝") ||
		!strings.Contains(err.Error(), "卖出成交价 9 低于限价 10") {
		t.Fatalf("错误应说明成交价格低于当前限价: %q", err.Error())
	}
	if res != (FillResult{}) {
		t.Fatalf("被拒绝成交必须返回零值成交结果，实际 %+v", res)
	}

	// 只追加一条拒绝记录，并保留本次回报的订单编号、成交编号、合约、方向、
	// 价格与数量；原因明确指出成交价 9 低于当前限价 10。
	recs := e.Records()
	if len(recs) != before.recs+1 {
		t.Fatalf("拒绝只能新增一条记录，实际新增 %d 条", len(recs)-before.recs)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rj)
	}
	if rj.OrderID != target || rj.TradeID != 3 || rj.Symbol != "A" ||
		rj.Side != Sell || rj.TradePrice != 9 || rj.Qty != 2 {
		t.Fatalf("拒绝记录必须保留本次回报的订单/成交编号、合约、方向、价格、数量: %+v", rj)
	}
	if rj.Filled != 0 || rj.Remaining != 0 {
		t.Fatalf("拒绝记录不得携带任何成交量: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "卖出成交价 9 低于限价 10") {
		t.Fatalf("拒绝原因须说明成交价格低于当前限价: %q", rj.Reason)
	}

	// 不扣减持仓、不增加现金、不释放卖单占用：现金 127、持仓 7、可卖 2 全部保持。
	after := snapshotSellRaisedLimitLedger(t, e, target, other)
	if after.cash != before.cash || after.position != before.position ||
		after.sellable != before.sellable {
		t.Fatalf("拒绝不得改变现金、持仓或可卖数量:\n之前 %+v\n之后 %+v", before, after)
	}
	if !reflect.DeepEqual(after.target, before.target) ||
		!reflect.DeepEqual(after.other, before.other) {
		t.Fatalf("拒绝不得改变两张订单:\n之前 target=%+v other=%+v\n之后 target=%+v other=%+v",
			before.target, before.other, after.target, after.other)
	}

	// 把回报价格修正为 10，沿用刚被拒绝的编号 3 和数量 2：被拒绝不占用编号，
	// 应正常入账。
	corrected := Trade{TradeID: 3, OrderID: target, Symbol: "A", Side: Sell, Price: 10, Qty: 2}
	res, err = e.Fill(corrected)
	if err != nil {
		t.Fatalf("修正价格到 10 后沿用编号 3 应正常入账: %v", err)
	}
	if res.TradeID != 3 || res.OrderID != target || res.Price != 10 || res.Qty != 2 ||
		res.Filled != 5 || res.Remaining != 1 || res.Status != StatusPartial {
		t.Fatalf("修正后成交结果应为累计 5、剩余 1、部分成交: %+v", res)
	}

	// 现金 127+20=147，持仓 7-2=5；目标订单累计成交 5、剩余 1；可卖数量仍为 2
	// （目标剩余 1 + 另一张卖单 2 = 3 份占用，持仓 5，可卖 5-3=2）。
	if e.Cash() != 147 || e.Position("A") != 5 || e.Sellable("A") != 2 {
		t.Fatalf("修正成交入账后账务错误: cash=%d pos=%d sellable=%d（应为 147/5/2）",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if to, _ := e.Order(target); to.Qty != 6 || to.Limit != 10 || to.Filled != 5 ||
		to.Status != StatusPartial || to.Remaining() != 1 {
		t.Fatalf("目标订单应累计成交 5、剩余 1、仍为部分成交，总量 6 限价 10 不变: %+v", to)
	}
	if oo, _ := e.Order(other); oo.Qty != 2 || oo.Limit != 8 || oo.Filled != 0 ||
		oo.Status != StatusPending || oo.Remaining() != 2 {
		t.Fatalf("另一张卖单必须始终保留自己的 2 份占用: %+v", oo)
	}

	// 先前的拒绝记录保留，新入账只在其后追加一条成交记录。
	recs = e.Records()
	if len(recs) != before.recs+2 {
		t.Fatalf("修正成交后应只比重放前多两条记录（拒绝+成交），实际 %d 条",
			len(recs)-before.recs)
	}
	if !reflect.DeepEqual(recs[before.recs], rj) {
		t.Fatalf("先前的拒绝记录必须原样保留:\nwant %+v\ngot  %+v", rj, recs[before.recs])
	}
	fr := recs[len(recs)-1]
	if fr.Kind != RecordFilled || fr.OrderID != target || fr.TradeID != 3 ||
		fr.Symbol != "A" || fr.Side != Sell || fr.TradePrice != 10 || fr.Qty != 2 ||
		fr.Filled != 5 || fr.Remaining != 1 {
		t.Fatalf("新成交记录错误: %+v", fr)
	}
}
