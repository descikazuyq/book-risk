package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“卖单调高限价后，历史成交仍然有效”补充自动化回归保障，明确区分
// 同一笔成交的再次提交（幂等重放）与尚未入账的新成交（按处理时的有效参数
// 判断），同时保留 Modify 修改订单与 Fill 逐笔结算的公开行为不变。
//
// 实际操作链条：
//
//  1. 账户现金 200：先买入同一合约 10 份、每份 10，并立即按 10 全部成交，
//     现金 100、持仓 10。
//  2. 接受目标卖单：总量 8、限价 8；另一张卖单占用余下 2 份可卖数量。
//  3. 目标卖单先以价格 9 成交 3 份：现金 127、持仓 7，目标订单累计成交
//     3 份、剩余 5 份（部分成交），两张卖单占满全部可卖数量。
//  4. 目标卖单改为总量 6、限价 10：已成交的 3 份不重新计价，现金与持仓
//     不变，剩余 3 份，可卖数量变为 2；另一张卖单仍保留自己的 2 份占用。
//
// 随后验证两个必须明确区分的时点：
//
//   - 用原成交编号、完全相同内容（价格 9、数量 3）再次提交：即使 9 已低于
//     新限价 10，也必须成功返回第一次的成交结果（累计成交 3、剩余 5、部分
//     成交），不按新限价拒绝、不把当前剩余 3 回填进历史结果，现金、持仓、
//     可卖数量、两张订单与记录数量全部保持重放前的值，订单查询仍显示修改
//     后的总量 6、限价 10。
//   - 用尚未成功入账的新编号提交价格 9、数量 2 的新成交：整笔拒绝，返回
//     错误与零值成交结果，只追加一条说明“卖出成交价低于当前限价”的拒绝
//     记录，并保留本次回报的订单编号、成交编号、合约、方向、价格与数量；
//     不扣减持仓、不增加现金、不释放任何卖单占用。把该回报价格修正为 10，
//     沿用刚被拒绝的编号与数量，应正常入账：现金 147、持仓 5，目标订单
//     累计成交 5、剩余 1（仍为部分成交），可卖数量仍为 2，另一张卖单不受
//     影响；先前的拒绝记录保留，新入账只追加对应的成交记录。

// sellReplayLedger 是重放/拒绝前后必须保持一致的账户与订单快照。
type sellReplayLedger struct {
	cash         int64
	position     int64
	sellable     int64
	targetOrder  Order
	otherOrder   Order
	recordCount  int
}

func snapshotSellReplayLedger(t *testing.T, e *Engine, target, other int64) sellReplayLedger {
	t.Helper()
	to, ok1 := e.Order(target)
	oo, ok2 := e.Order(other)
	if !ok1 || !ok2 {
		t.Fatalf("两张卖单都必须能查询到: target=%v other=%v", ok1, ok2)
	}
	return sellReplayLedger{
		cash:        e.Cash(),
		position:    e.Position("A"),
		sellable:    e.Sellable("A"),
		targetOrder: to,
		otherOrder:  oo,
		recordCount: len(e.Records()),
	}
}

// TestSellFillReplayAfterLimitRaisedVersusNewFill 完整走一遍上述链条：
// 调高部分成交卖单的限价后，旧成交低价重放仍返回第一次结果，新成交则按
// 新限价判断（先拒绝、修正价格后同编号入账）。
func TestSellFillReplayAfterLimitRaisedVersusNewFill(t *testing.T) {
	e, _ := NewEngine(200)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	// 先买入 10 份、限价 10，并立即按 10 全部成交：现金 200-100=100，持仓 10。
	buyID := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: buyID, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatalf("建仓买入应全部成交: %v", err)
	}
	if e.Cash() != 100 || e.Position("A") != 10 || e.ReservedCash() != 0 || e.Sellable("A") != 10 {
		t.Fatalf("建仓后状态错误: cash=%d pos=%d reserved=%d sellable=%d",
			e.Cash(), e.Position("A"), e.ReservedCash(), e.Sellable("A"))
	}

	// 目标卖单：总量 8、限价 8，占用 8 份可卖；另一张卖单占用余下 2 份。
	target := mustSell(t, e, "A", 8, 8)
	other := mustSell(t, e, "A", 2, 8)
	if e.Sellable("A") != 0 {
		t.Fatalf("两张卖单应占满全部可卖数量，实际可卖 %d", e.Sellable("A"))
	}

	// 目标卖单先以价格 9 成交 3 份：现金 100+27=127，持仓 7；目标订单累计
	// 成交 3、剩余 5、部分成交；本单未成交占用由 8 降为 5，另一张卖单仍占 2，
	// 可卖数量 7-5-2=0。
	first := Trade{TradeID: 2, OrderID: target, Symbol: "A", Side: Sell, Price: 9, Qty: 3}
	resFirst, err := e.Fill(first)
	if err != nil {
		t.Fatalf("目标卖单首次部分成交应成功: %v", err)
	}
	if resFirst.TradeID != 2 || resFirst.OrderID != target || resFirst.Price != 9 || resFirst.Qty != 3 ||
		resFirst.Filled != 3 || resFirst.Remaining != 5 || resFirst.Status != StatusPartial {
		t.Fatalf("第一次成交结果错误: %+v", resFirst)
	}
	if e.Cash() != 127 || e.Position("A") != 7 || e.Sellable("A") != 0 {
		t.Fatalf("首次卖出成交后状态错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(target); o.Limit != 8 || o.Qty != 8 || o.Filled != 3 ||
		o.Remaining() != 5 || o.Status != StatusPartial {
		t.Fatalf("目标订单修改前状态错误: %+v", o)
	}
	if o, _ := e.Order(other); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("另一张卖单修改前应原样保留 2 份占用: %+v", o)
	}

	// 把目标卖单改为总量 6、限价 10：新剩余 = 6-3 = 3 替换旧剩余 5 的占用；
	// 历史成交不重新计价，现金与持仓不变；可卖数量 7-3-2=2。
	recordsBeforeModify := len(e.Records())
	mustModify(t, e, target, 6, 10)
	if e.Cash() != 127 || e.Position("A") != 7 || e.Sellable("A") != 2 {
		t.Fatalf("修改卖单不得重新计价历史成交: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(target); o.Qty != 6 || o.Limit != 10 || o.Filled != 3 ||
		o.Remaining() != 3 || o.Status != StatusPartial {
		t.Fatalf("目标订单修改后应为总量 6、限价 10、累计成交 3、剩余 3: %+v", o)
	}
	if o, _ := e.Order(other); o.Status != StatusPending || o.Qty != 2 ||
		o.Filled != 0 || o.Limit != 8 || o.Remaining() != 2 {
		t.Fatalf("修改目标卖单不得影响另一张卖单的 2 份占用: %+v", o)
	}
	if len(e.Records()) != recordsBeforeModify+1 {
		t.Fatalf("真实修改只能新增一条修改记录，实际 %d 条", len(e.Records())-recordsBeforeModify)
	}
	if mod := e.Records()[len(e.Records())-1]; mod.Kind != RecordModified ||
		mod.OldQty != 8 || mod.OldLimit != 8 || mod.OldFilled != 3 ||
		mod.Qty != 6 || mod.Limit != 10 || mod.Filled != 3 || mod.Remaining != 3 {
		t.Fatalf("修改记录前后参数错误: %+v", mod)
	}

	// ---- 同一笔成交的再次提交：原编号、完全相同内容（价格 9 已低于新限价 10）。
	replaySnapshot := snapshotSellReplayLedger(t, e, target, other)
	recordsAtReplay := e.Records()

	replay, err := e.Fill(first)
	if err != nil {
		t.Fatalf("已入账成交的同内容重放即使价格低于新限价也必须成功: %v", err)
	}
	// 必须原样返回第一次的成交结果：累计成交 3、剩余 5、部分成交——
	// 不能按新限价拒绝，也不能把当前剩余 3 回填进历史结果。
	if replay != resFirst {
		t.Fatalf("重放必须返回第一次成交结果: want %+v got %+v", resFirst, replay)
	}
	if replay.Filled != 3 || replay.Remaining != 5 || replay.Status != StatusPartial {
		t.Fatalf("重放结果必须保留首次入账时点: filled=%d remaining=%d status=%s",
			replay.Filled, replay.Remaining, replay.Status)
	}

	// 现金、持仓、可卖数量、两张订单与记录数量均保持重放前的值。
	if got := snapshotSellReplayLedger(t, e, target, other); got != replaySnapshot {
		t.Fatalf("幂等重放不得改变任何账实或订单:\n之前 %+v\n之后 %+v", replaySnapshot, got)
	}
	if len(e.Records()) != replaySnapshot.recordCount {
		t.Fatalf("幂等重放不得新增记录，实际 %d 条", len(e.Records()))
	}
	for i, want := range recordsAtReplay {
		if !reflect.DeepEqual(e.Records()[i], want) {
			t.Fatalf("第 %d 条历史记录被重放改写:\nwant %+v\ngot  %+v", i, want, e.Records()[i])
		}
	}
	// 订单查询继续显示修改后的总量 6 与限价 10，而不是旧结果中的 8/8。
	if o, _ := e.Order(target); o.Qty != 6 || o.Limit != 10 || o.Filled != 3 ||
		o.Remaining() != 3 || o.Status != StatusPartial {
		t.Fatalf("重放后订单查询仍须显示修改后的参数: %+v", o)
	}

	// ---- 尚未入账的新成交（新编号）：价格 9 低于当前限价 10，整笔拒绝。
	recsBeforeReject := len(e.Records())
	low := Trade{TradeID: 3, OrderID: target, Symbol: "A", Side: Sell, Price: 9, Qty: 2}
	resReject, err := e.Fill(low)
	if err == nil {
		t.Fatalf("价格 9 低于新限价 10 的新成交必须报错")
	}
	for _, s := range []string{"成交 3 被拒绝", "卖出成交价 9", "低于限价 10"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("错误 %q 应包含 %q", err.Error(), s)
		}
	}
	if resReject != (FillResult{}) {
		t.Fatalf("拒绝必须返回零值成交结果，实际 %+v", resReject)
	}

	// 只追加一条拒绝记录，并保留本次回报的订单编号、成交编号、合约、方向、
	// 价格与数量；全部场景未开启交易日，记录不得携带风险快照。
	recs := e.Records()
	if len(recs) != recsBeforeReject+1 {
		t.Fatalf("低价新成交只能新增一条拒绝记录，实际 %d 条", len(recs)-recsBeforeReject)
	}
	rej := recs[len(recs)-1]
	if rej.Kind != RecordRejected || rej.OrderID != target || rej.TradeID != 3 ||
		rej.Symbol != "A" || rej.Side != Sell || rej.TradePrice != 9 || rej.Qty != 2 {
		t.Fatalf("拒绝记录必须完整保留本次回报的编号、合约、方向、价格与数量: %+v", rej)
	}
	if !strings.Contains(rej.Reason, "卖出成交价 9") || !strings.Contains(rej.Reason, "低于限价 10") {
		t.Fatalf("拒绝记录原因必须说明成交价低于当前限价: %q", rej.Reason)
	}
	if !rej.QuoteValid || rej.QuoteSeq != 1 || rej.QuotePrice != 10 ||
		!rej.MaxPositionValid || rej.MaxPosition != 100 {
		t.Fatalf("拒绝记录必须固化当时报价与持仓限额快照: %+v", rej)
	}
	if rej.RiskDay != 0 || rej.RiskRestrict {
		t.Fatalf("未开日时拒绝记录不应携带风险快照: %+v", rej)
	}

	// 拒绝不得扣减持仓、增加现金或释放卖单占用。
	if e.Cash() != 127 || e.Position("A") != 7 || e.Sellable("A") != 2 {
		t.Fatalf("低价拒绝不得改变资金、持仓或可卖占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(target); o.Qty != 6 || o.Limit != 10 || o.Filled != 3 ||
		o.Remaining() != 3 || o.Status != StatusPartial {
		t.Fatalf("低价拒绝不得改变目标订单: %+v", o)
	}
	if o, _ := e.Order(other); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("低价拒绝不得影响另一张卖单的 2 份占用: %+v", o)
	}

	// ---- 价格修正为 10，沿用刚被拒绝的编号 3 与数量 2：正常入账。
	resOK, err := e.Fill(Trade{TradeID: 3, OrderID: target, Symbol: "A", Side: Sell, Price: 10, Qty: 2})
	if err != nil {
		t.Fatalf("修正价格后沿用原编号应正常入账: %v", err)
	}
	// 现金 127+20=147，持仓 7-2=5；目标订单累计成交 5、剩余 1、仍部分成交。
	if resOK.TradeID != 3 || resOK.OrderID != target || resOK.Price != 10 || resOK.Qty != 2 ||
		resOK.Filled != 5 || resOK.Remaining != 1 || resOK.Status != StatusPartial {
		t.Fatalf("修正后成交结果错误: %+v", resOK)
	}
	if e.Cash() != 147 || e.Position("A") != 5 {
		t.Fatalf("修正后应按价格 10 结算: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	// 目标单未成交占用 3-2=1，另一张仍占 2：可卖数量 5-1-2=2，保持不变。
	if e.Sellable("A") != 2 {
		t.Fatalf("修正成交后可卖数量应仍为 2（目标剩 1 + 另一单 2），实际 %d", e.Sellable("A"))
	}
	if o, _ := e.Order(target); o.Qty != 6 || o.Limit != 10 || o.Filled != 5 ||
		o.Remaining() != 1 || o.Status != StatusPartial {
		t.Fatalf("目标订单应累计成交 5、剩余 1、仍为部分成交，且保留修改后的总量/限价: %+v", o)
	}
	if o, _ := e.Order(other); o.Status != StatusPending || o.Qty != 2 ||
		o.Filled != 0 || o.Limit != 8 || o.Remaining() != 2 {
		t.Fatalf("另一张卖单全程不受影响: %+v", o)
	}

	// 先前的拒绝记录保留，新入账只在其后追加一条成交记录。
	recs = e.Records()
	if len(recs) != recsBeforeReject+2 {
		t.Fatalf("修正成交后应只比拒绝前多两条记录（拒绝+成交），实际 %d 条",
			len(recs)-recsBeforeReject)
	}
	if !reflect.DeepEqual(recs[recsBeforeReject], rej) {
		t.Fatalf("先前的拒绝记录必须原样保留: want %+v got %+v", rej, recs[recsBeforeReject])
	}
	fill := recs[recsBeforeReject+1]
	if fill.Kind != RecordFilled || fill.OrderID != target || fill.TradeID != 3 ||
		fill.Symbol != "A" || fill.Side != Sell || fill.TradePrice != 10 ||
		fill.Qty != 2 || fill.Filled != 5 || fill.Remaining != 1 {
		t.Fatalf("修正后的成交记录错误: %+v", fill)
	}

	// 入账后的同内容重放仍返回本次结果，且不再新增记录、不重复结算。
	again, err := e.Fill(Trade{TradeID: 3, OrderID: target, Symbol: "A", Side: Sell, Price: 10, Qty: 2})
	if err != nil || again != resOK {
		t.Fatalf("入账成交重放应返回同一结果: %+v, %v", again, err)
	}
	if len(e.Records()) != recsBeforeReject+2 || e.Cash() != 147 ||
		e.Position("A") != 5 || e.Sellable("A") != 2 {
		t.Fatalf("入账成交重放不得新增记录或重复结算: recs=%d cash=%d pos=%d sellable=%d",
			len(e.Records()), e.Cash(), e.Position("A"), e.Sellable("A"))
	}
}
