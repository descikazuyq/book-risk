package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“买单部分成交后同时下调总量与限价，已入账成交回报的幂等重放与
// 尚未入账新成交的拒绝口径”补充回归保障。买单先以较高限价部分成交，随后把
// 总量与限价同时下调到旧成交数量高于新剩余量、旧成交价高于新限价的程度：
// 修改只替换未成交部分的占用，不重算原先成交的成本。此时以原成交编号、原订单、
// 原合约、原方向、原价格和原数量再次提交成交，必须原样返回第一次的成交结果
// （第一次入账时的本次成交量、累计成交量、剩余量与部分成交状态），不能因价格
// 高于新限价或数量超过新剩余量而拒绝，也不能把修改后的订单现状回填到历史结果；
// 现金、持仓、现金占用、可用现金与记录数量保持重放前的值，订单查询继续显示
// 修改后的总量与限价，历史返回结果不能覆盖订单现状。
//
// 同一个已入账编号若只把价格改成符合当前限价的新值（其余字段不变），即使新内容
// 本身符合当前限价，也必须报成交编号内容冲突：返回错误与零值成交结果，只追加
// 一条保留本次回报及冲突原因的拒绝记录（原因指向第一次成交的原始内容），现金、
// 持仓、占用与订单全部不变。尚未入账的新成交按处理时的有效参数判断：价格高于
// 当前限价时整笔拒绝，只留下说明价格高于限价的拒绝记录，返回零值结果，不扣现金、
// 不增持仓、不释放占用；被拒绝不占用成交编号，把价格修正到不高于当前限价后沿用
// 同一编号应正常成交，先前的拒绝记录保留，新成交只追加对应的成交记录。整个过程
// 不开启日内亏损保护与账户总持仓金额上限，不改变 Modify 与 Fill 的公开行为。

// buyLoweredLimitLedger 是关键操作前后必须保持一致的账户与订单快照。
type buyLoweredLimitLedger struct {
	cash      int64
	reserved  int64
	available int64
	position  int64
	order     Order
	recs      int
}

func snapshotBuyLoweredLimitLedger(t *testing.T, e *Engine, orderID int64) buyLoweredLimitLedger {
	t.Helper()
	o, ok := e.Order(orderID)
	if !ok {
		t.Fatalf("订单 %d 快照缺失", orderID)
	}
	return buyLoweredLimitLedger{
		cash:      e.Cash(),
		reserved:  e.ReservedCash(),
		available: e.AvailableCash(),
		position:  e.Position("A"),
		order:     o,
		recs:      len(e.Records()),
	}
}

// setupBuyLoweredLimitReplay 准备题述场景（不开日内亏损保护、不设金额上限、
// 行情连续）：现金 1000、合约 A 最大持仓 100、已生效报价序号 1（时刻 10、
// 价格 10）；接受总量 10、限价 10 的买单（订单 1），以成交编号 1 按价格 9
// 成交 4 份，再把订单修改为总量 6、限价 7。
//
// 首次成交按实际价格结算：现金 1000-9×4=964，持仓 4；成交时按原限价 10
// 释放 4 份占用（100-40=60）。修改只替换未成交部分的占用：剩余由 6 份改为
// 2 份、限价由 10 改为 7，占用 60-60+14=14，可用现金 964-14=950；原先 4 份
// 成交的成本不重算（现金仍为 964，而不是按新限价重算后的 976）。
//
// 返回引擎、买单编号与第一次成交（编号 1、价格 9、数量 4）的成交结果。
func setupBuyLoweredLimitReplay(t *testing.T) (e *Engine, orderID int64, first FillResult) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	orderID = mustBuy(t, e, "A", 10, 10)

	// 成交编号 1 按价格 9 成交 4 份：现金 964、持仓 4；按原限价 10 释放
	// 4 份现金占用，剩余 6 份继续占用 60，可用现金 904。
	firstTrade := Trade{TradeID: 1, OrderID: orderID, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	first, err := e.Fill(firstTrade)
	if err != nil {
		t.Fatalf("买单首次部分成交应成功: %v", err)
	}
	if first.TradeID != 1 || first.OrderID != orderID || first.Price != 9 || first.Qty != 4 ||
		first.Filled != 4 || first.Remaining != 6 || first.Status != StatusPartial {
		t.Fatalf("第一次成交结果错误: %+v", first)
	}
	if e.Cash() != 964 || e.ReservedCash() != 60 || e.AvailableCash() != 904 || e.Position("A") != 4 {
		t.Fatalf("首次成交后账务错误: cash=%d reserved=%d available=%d pos=%d（应为 964/60/904/4）",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if o, _ := e.Order(orderID); o.Qty != 10 || o.Limit != 10 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("订单首次成交后状态错误: %+v", o)
	}

	// 同时下调总量到 6、限价到 7：已成交 4 份不动，剩余量 6→2；旧成交成本
	// 不重算，现金 964、持仓 4 不变；未成交占用只按新限价 7 × 新剩余 2 = 14
	// 替换原先 10 × 6 = 60，可用现金 950。
	mustModify(t, e, orderID, 6, 7)
	if e.Cash() != 964 || e.ReservedCash() != 14 || e.AvailableCash() != 950 || e.Position("A") != 4 {
		t.Fatalf("修改后账务错误: cash=%d reserved=%d available=%d pos=%d（应为 964/14/950/4）",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, _ := e.Order(orderID)
	if o.Qty != 6 || o.Limit != 7 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 2 {
		t.Fatalf("订单修改后应为总量 6、限价 7、累计成交 4、有效剩余 2: %+v", o)
	}

	// 记录到修改为止共 3 条：接受、成交、修改。
	if recs := e.Records(); len(recs) != 3 {
		t.Fatalf("修改后应有 3 条记录（接受、成交、修改），实际 %d", len(recs))
	}
	return e, orderID, first
}

// TestBuyLoweredLimitHistoricalFillReplayRegression 覆盖题述核心：买单部分成交后
// 同时把总量由 10 下调到 6、限价由 10 下调到 7，旧成交 4 份已超过现在的剩余量
// 2、旧成交价 9 也高于现在的限价 7。此时以原成交编号、原订单、原合约、原方向、
// 原价格 9 和原数量 4 再次提交成交，必须原样返回第一次的成交结果（本次成交 4、
// 累计成交 4、当时剩余 6、部分成交），不能因订单的新参数被拒绝；现金、持仓、
// 占用、可用现金与记录数量全部保持重放前的值，订单查询继续显示修改后的总量 6、
// 限价 7、已成交 4、有效剩余 2，历史返回结果不能覆盖订单现状，原有成交记录与
// 修改记录保持各自发生时的内容。
func TestBuyLoweredLimitHistoricalFillReplayRegression(t *testing.T) {
	e, orderID, first := setupBuyLoweredLimitReplay(t)
	firstTrade := Trade{TradeID: 1, OrderID: orderID, Symbol: "A", Side: Buy, Price: 9, Qty: 4}

	before := snapshotBuyLoweredLimitLedger(t, e, orderID)
	if before.cash != 964 || before.reserved != 14 || before.available != 950 || before.position != 4 {
		t.Fatalf("重放前账务应为现金 964、占用 14、可用 950、持仓 4: %+v", before)
	}
	if before.order.Qty != 6 || before.order.Limit != 7 || before.order.Filled != 4 ||
		before.order.Remaining() != 2 || before.order.Status != StatusPartial {
		t.Fatalf("重放前订单应为总量 6、限价 7、已成交 4、剩余 2: %+v", before.order)
	}
	if before.recs != 3 {
		t.Fatalf("重放前应有 3 条记录（接受、成交、修改），实际 %d", before.recs)
	}
	recordsBefore := e.Records()

	// 原样重提：旧成交数量 4 超过现在剩余量 2、旧成交价 9 高于现在限价 7，
	// 两项按新参数都该拒绝，但已入账成交必须走幂等重放而不是重新校验。
	replay, err := e.Fill(firstTrade)
	if err != nil {
		t.Fatalf("已入账成交以相同内容重放不应报错（即使价格 9 高于新限价 7、数量 4 超过新剩余 2）: %v", err)
	}
	if replay != first {
		t.Fatalf("重放必须原样返回第一次成交结果:\n第一次 %+v\n重放   %+v", first, replay)
	}
	// 结果必须定格在第一次入账时点：本次 4、累计 4、当时剩余 6、部分成交，
	// 绝不能回填修改后的剩余 2 或受新限价 7 影响。
	if replay.Qty != 4 || replay.Filled != 4 || replay.Remaining != 6 || replay.Status != StatusPartial {
		t.Fatalf("重放结果必须保留第一次入账时点的本次 4、累计 4、剩余 6、部分成交: %+v", replay)
	}

	// 重放不再次扣现金、不增加持仓、不释放占用：现金 964、持仓 4、占用 14、
	// 可用 950 全部保持；订单与记录数量也不变。
	after := snapshotBuyLoweredLimitLedger(t, e, orderID)
	if after != before {
		t.Fatalf("幂等重放不得改变任何账户或订单状态:\n之前 %+v\n之后 %+v", before, after)
	}

	// 历史返回结果不能覆盖订单现状：订单查询继续显示修改后的总量 6、限价 7、
	// 已成交 4、有效剩余 2、部分成交。
	o, _ := e.Order(orderID)
	if o.Qty != 6 || o.Limit != 7 || o.Filled != 4 || o.Remaining() != 2 || o.Status != StatusPartial {
		t.Fatalf("重放后订单查询仍须显示修改后的总量 6、限价 7、已成交 4、剩余 2: %+v", o)
	}

	// 原有成交记录和修改记录保持各自发生时的内容，重放不新增任何记录。
	recs := e.Records()
	if len(recs) != before.recs {
		t.Fatalf("重放不得新增记录，实际 %d 条", len(recs))
	}
	for i, want := range recordsBefore {
		if !reflect.DeepEqual(recs[i], want) {
			t.Fatalf("第 %d 条原有记录被重放改写:\nwant %+v\ngot  %+v", i, want, recs[i])
		}
	}
	// 成交记录保留第一次入账时点（累计 4、剩余 6、成交价 9），修改记录保留
	// 修改时点（新总量 6、新限价 7、剩余 2、旧总量 10、旧限价 10）。
	fillRec := recs[1]
	if fillRec.Kind != RecordFilled || fillRec.TradeID != 1 || fillRec.TradePrice != 9 ||
		fillRec.Qty != 4 || fillRec.Filled != 4 || fillRec.Remaining != 6 {
		t.Fatalf("原成交记录必须保持第一次入账时的内容: %+v", fillRec)
	}
	modRec := recs[2]
	if modRec.Kind != RecordModified || modRec.Qty != 6 || modRec.Limit != 7 ||
		modRec.Filled != 4 || modRec.Remaining != 2 || modRec.OldQty != 10 || modRec.OldLimit != 10 {
		t.Fatalf("修改记录必须保持修改发生时的内容: %+v", modRec)
	}
}

// TestBuyLoweredLimitHistoricalFillContentConflictRegression 覆盖同一个已入账编号
// 被改成不同内容重提：把编号 1 的价格由 9 改为 7（即使新价格恰好符合当前限价 7，
// 订单、合约、方向与数量都与第一次相同），也必须报成交编号内容冲突而不是按新
// 限价接受；返回零值成交结果，只追加一条保留本次回报及冲突原因的拒绝记录
// （原因明确指出编号 1 已用于价格 9、数量 4 的成交），现金、持仓、占用与订单
// 全部不变，原有成交记录与修改记录原样保留。
func TestBuyLoweredLimitHistoricalFillContentConflictRegression(t *testing.T) {
	e, orderID, first := setupBuyLoweredLimitReplay(t)

	before := snapshotBuyLoweredLimitLedger(t, e, orderID)
	recordsBefore := e.Records()

	// 只把价格改成 7：新内容本身完全符合当前限价（7 不高于限价 7、数量 4 仍是
	// 已入账数量），但编号 1 的第一次成交价格是 9，必须只因编号内容冲突而拒绝。
	conflict := Trade{TradeID: 1, OrderID: orderID, Symbol: "A", Side: Buy, Price: 7, Qty: 4}
	res, err := e.Fill(conflict)
	if err == nil {
		t.Fatal("已入账编号以不同价格重提必须报错")
	}
	wantSubs := []string{
		"成交编号 1 已用于不同内容的成交",
		"订单=1",
		"合约=A",
		"方向=买入",
		"价格=9",
		"数量=4",
	}
	for _, s := range wantSubs {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("冲突错误 %q 应包含 %q", err.Error(), s)
		}
	}
	if res != (FillResult{}) {
		t.Fatalf("编号内容冲突必须返回零值成交结果，实际 %+v", res)
	}

	// 只追加一条拒绝记录：原样保留本次回报的订单编号、成交编号、合约、方向、
	// 价格 7 与数量 4，原因指向第一次成交的原始内容。
	recs := e.Records()
	if len(recs) != before.recs+1 {
		t.Fatalf("编号冲突只能新增一条拒绝记录，实际新增 %d 条", len(recs)-before.recs)
	}
	rec := recs[len(recs)-1]
	if rec.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rec)
	}
	if rec.OrderID != orderID || rec.TradeID != 1 || rec.Symbol != "A" ||
		rec.Side != Buy || rec.TradePrice != 7 || rec.Qty != 4 {
		t.Fatalf("拒绝记录必须保留本次提交的订单/成交编号、合约、方向、价格 7、数量 4: %+v", rec)
	}
	if rec.Filled != 0 || rec.Remaining != 0 {
		t.Fatalf("拒绝记录不得携带任何成交量: %+v", rec)
	}
	for _, s := range wantSubs {
		if !strings.Contains(rec.Reason, s) {
			t.Fatalf("拒绝原因 %q 应包含 %q", rec.Reason, s)
		}
	}
	// 全部场景均在未开启交易日、未设金额上限的账户中进行：拒绝记录不得携带
	// 任何风险或金额上限快照。
	if rec.RiskDay != 0 || rec.RiskRestrict || rec.AmtEnabled || rec.AmtLimit != 0 {
		t.Fatalf("未开日且未设金额上限时拒绝记录不应携带风险/金额快照: %+v", rec)
	}

	// 账户和订单不变：现金 964、占用 14、可用 950、持仓 4，订单仍为总量 6、
	// 限价 7、累计成交 4、剩余 2、部分成交；原有记录一条都不能被改写。
	// （recs 字段只用于记录条数核对，这里不参与状态相等比较。）
	after := snapshotBuyLoweredLimitLedger(t, e, orderID)
	after.recs = before.recs
	if after != before {
		t.Fatalf("编号冲突不得改变任何资金、持仓、占用或订单状态:\n之前 %+v\n之后 %+v", before, after)
	}
	for i, want := range recordsBefore {
		if !reflect.DeepEqual(recs[i], want) {
			t.Fatalf("第 %d 条原有记录被冲突回报改写:\nwant %+v\ngot  %+v", i, want, recs[i])
		}
	}

	// 冲突之后，原成交按原始内容（价格 9）重放仍返回第一次结果，不再次记账、
	// 不新增记录——冲突没有破坏幂等表中的第一次结果。
	replay, err := e.Fill(Trade{TradeID: 1, OrderID: orderID, Symbol: "A", Side: Buy, Price: 9, Qty: 4})
	if err != nil {
		t.Fatalf("冲突后原成交按原始内容重放不应报错: %v", err)
	}
	if replay != first {
		t.Fatalf("冲突后原成交重放仍须返回第一次结果: want %+v got %+v", first, replay)
	}
	if len(e.Records()) != before.recs+1 {
		t.Fatalf("原成交重放不得再新增记录，实际 %d 条", len(e.Records()))
	}
}

// TestBuyLoweredLimitNewFillRejectedThenCorrectedRegression 覆盖题述后半段：用一个
// 尚未入账的编号 2 按价格 9 成交 1 份，价格 9 高于当前限价 7，应整笔拒绝
// （错误与零值成交结果），只留下一条说明价格 9 高于限价 7 的拒绝记录并保留本次
// 回报全部字段，不扣现金、不增持仓、不释放占用；把这笔回报的价格修正为 7 后
// 沿用编号 2 应成功成交 1 份：现金 957、持仓 5、占用现金 7、可用 950，订单
// 累计成交 5、剩余 1、仍为部分成交。此前的拒绝记录保留，新成交只新增一笔成功
// 成交的记录。
func TestBuyLoweredLimitNewFillRejectedThenCorrectedRegression(t *testing.T) {
	e, orderID, _ := setupBuyLoweredLimitReplay(t)

	before := snapshotBuyLoweredLimitLedger(t, e, orderID)
	if before.cash != 964 || before.reserved != 14 || before.available != 950 || before.position != 4 {
		t.Fatalf("前置账务应为现金 964、占用 14、可用 950、持仓 4: %+v", before)
	}

	// 尚未入账的编号 2 按价格 9 成交 1 份：9 > 当前限价 7，必须整笔拒绝。
	rejected := Trade{TradeID: 2, OrderID: orderID, Symbol: "A", Side: Buy, Price: 9, Qty: 1}
	res, err := e.Fill(rejected)
	if err == nil {
		t.Fatal("新成交价格 9 高于当前限价 7 必须报错")
	}
	if !strings.Contains(err.Error(), "成交 2 被拒绝") ||
		!strings.Contains(err.Error(), "买入成交价 9 高于限价 7") {
		t.Fatalf("错误应说明成交 2 因价格 9 高于限价 7 被拒绝: %q", err.Error())
	}
	if res != (FillResult{}) {
		t.Fatalf("被拒绝成交必须返回零值成交结果，实际 %+v", res)
	}

	// 只追加一条拒绝记录，保留本次回报的订单编号、成交编号、合约、方向、价格 9
	// 与数量 1，原因明确指出价格 9 高于限价 7。
	recs := e.Records()
	if len(recs) != before.recs+1 {
		t.Fatalf("拒绝只能新增一条记录，实际新增 %d 条", len(recs)-before.recs)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rj)
	}
	if rj.OrderID != orderID || rj.TradeID != 2 || rj.Symbol != "A" ||
		rj.Side != Buy || rj.TradePrice != 9 || rj.Qty != 1 {
		t.Fatalf("拒绝记录必须保留本次回报的订单/成交编号、合约、方向、价格 9、数量 1: %+v", rj)
	}
	if rj.Filled != 0 || rj.Remaining != 0 {
		t.Fatalf("拒绝记录不得携带任何成交量: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "买入成交价 9 高于限价 7") {
		t.Fatalf("拒绝原因须说明价格 9 高于当前限价 7: %q", rj.Reason)
	}

	// 不扣现金、不增持仓、不释放占用：现金 964、持仓 4、占用 14、可用 950，
	// 订单累计成交仍是 4、剩余 2。（recs 字段只用于记录条数核对，不参与这里的
	// 状态相等比较。）
	after := snapshotBuyLoweredLimitLedger(t, e, orderID)
	after.recs = before.recs
	if after != before {
		t.Fatalf("拒绝不得改变现金、持仓、占用或订单:\n之前 %+v\n之后 %+v", before, after)
	}

	// 把这笔新回报的价格修正为 7，沿用编号 2 成交 1 份：被拒绝不占用编号，
	// 应正常入账。现金 964-7=957，持仓 4+1=5；按当前限价 7 释放 1 份占用，
	// 占用 14-7=7，可用现金 957-7=950。
	corrected := Trade{TradeID: 2, OrderID: orderID, Symbol: "A", Side: Buy, Price: 7, Qty: 1}
	res, err = e.Fill(corrected)
	if err != nil {
		t.Fatalf("修正价格到 7 后沿用编号 2 应正常成交: %v", err)
	}
	if res.TradeID != 2 || res.OrderID != orderID || res.Price != 7 || res.Qty != 1 ||
		res.Filled != 5 || res.Remaining != 1 || res.Status != StatusPartial {
		t.Fatalf("修正后成交结果应为本次 1、累计 5、剩余 1、部分成交: %+v", res)
	}
	if e.Cash() != 957 || e.ReservedCash() != 7 || e.AvailableCash() != 950 || e.Position("A") != 5 {
		t.Fatalf("修正成交入账后账务错误: cash=%d reserved=%d available=%d pos=%d（应为 957/7/950/5）",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, _ := e.Order(orderID)
	if o.Qty != 6 || o.Limit != 7 || o.Filled != 5 || o.Remaining() != 1 || o.Status != StatusPartial {
		t.Fatalf("订单应累计成交 5、剩余 1、仍为部分成交，总量 6 限价 7 不变: %+v", o)
	}

	// 此前的拒绝记录保留，新成交只在其后追加一条成交记录（记录固化成交时点：
	// 累计 5、剩余 1、成交价 7）。
	recs = e.Records()
	if len(recs) != before.recs+2 {
		t.Fatalf("修正成交后应只比拒绝前多两条记录（拒绝+成交），实际 %d 条",
			len(recs)-before.recs)
	}
	if !reflect.DeepEqual(recs[before.recs], rj) {
		t.Fatalf("先前的拒绝记录必须原样保留:\nwant %+v\ngot  %+v", rj, recs[before.recs])
	}
	fr := recs[len(recs)-1]
	if fr.Kind != RecordFilled || fr.OrderID != orderID || fr.TradeID != 2 ||
		fr.Symbol != "A" || fr.Side != Buy || fr.TradePrice != 7 || fr.Qty != 1 ||
		fr.Filled != 5 || fr.Remaining != 1 {
		t.Fatalf("新成交记录错误: %+v", fr)
	}
}
