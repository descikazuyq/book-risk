package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“买单部分成交后同时下调总量与限价，已入账成交按原内容重放仍返回第一次
// 结果”补充回归保障。已有功能允许修改有效订单的总量与限价，但已经入账的成交再次
// 提交时必须返回第一次的成交结果，不能因订单的新参数被拒绝：旧成交的数量（4）超过
// 修改后的剩余量（2）、旧成交的价格（9）高于修改后的限价（7），原样重提仍然成功。
//
// 场景账户：现金 1000、合约 A 最大持仓 100、已生效报价序号 1（时刻 10、价格 10），
// 不开启日内亏损保护和账户总持仓金额上限。接受总量 10、限价 10 的买单，以成交编号 1
// 按价格 9 成交 4 份，再将订单修改为总量 6、限价 7：现金余额 964、持仓 4、剩余量 2、
// 占用现金 14、可用现金 950——修改只替换未成交部分的占用，不重算原先 4 份成交的成本。
//
// 随后按原成交编号、原订单、原合约、原方向、原价格和原数量再次提交成交：返回结果
// 须与首次完全相同（本次成交 4 份、累计成交 4 份、当时剩余 6 份、部分成交），不再
// 次扣现金、增加持仓、释放占用或新增事件；订单查询继续显示修改后的总量 6、限价 7、
// 已成交 4、有效剩余 2，历史返回结果不能覆盖订单现状；原有成交记录与修改记录保持
// 各自发生时的内容。
//
// 同一已入账编号若把价格改为 7，即使新内容符合当前限价，也应报成交编号内容冲突，
// 只追加一条保留这次回报及冲突原因的拒绝记录，账户和订单不变。另用尚未入账的编号 2
// 按价格 9 成交 1 份，应因超过当前限价 7 被拒绝，返回零值成交结果，仅留下说明
// “价格 9 高于限价 7”的拒绝记录；把这笔新回报的价格修正为 7 后，仍可沿用编号 2
// 成功成交 1 份：现金 957、持仓 5、占用现金 7，订单累计成交 5、剩余 1，仍为部分
// 成交；此前的拒绝记录保留，只新增这笔成功成交的记录。

// modifyReplayLedger 是重放/冲突/拒绝前后必须保持一致的账户与订单快照。
type modifyReplayLedger struct {
	cash      int64
	reserved  int64
	available int64
	position  int64
	order     Order
}

func snapshotModifyReplayLedger(e *Engine, orderID int64, symbol string) modifyReplayLedger {
	o, _ := e.Order(orderID)
	return modifyReplayLedger{
		cash:      e.Cash(),
		reserved:  e.ReservedCash(),
		available: e.AvailableCash(),
		position:  e.Position(symbol),
		order:     o,
	}
}

// TestFillReplayAfterModifyLowerQtyAndLimit 覆盖主场景：部分成交后同时下调总量与
// 限价，旧成交的数量超过现在的剩余量、价格也高于现在的限价，按原内容重提仍返回
// 第一次的成交结果，不被订单的新参数拒绝。
func TestFillReplayAfterModifyLowerQtyAndLimit(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	// 接受总量 10、限价 10 的买单：占用现金 100，现金余额仍为 1000。
	bid := mustBuy(t, e, "A", 10, 10)
	if e.Cash() != 1000 || e.ReservedCash() != 100 || e.AvailableCash() != 900 {
		t.Fatalf("下单后账务错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}

	// 成交编号 1 按价格 9 成交 4 份：现金 964、持仓 4、剩余量 6、占用现金 60。
	first := Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	res1, err := e.Fill(first)
	if err != nil {
		t.Fatalf("首次部分成交应成功: %v", err)
	}
	if res1.TradeID != 1 || res1.OrderID != bid || res1.Price != 9 || res1.Qty != 4 ||
		res1.Filled != 4 || res1.Remaining != 6 || res1.Status != StatusPartial {
		t.Fatalf("首次成交结果错误: %+v", res1)
	}
	if e.Cash() != 964 || e.ReservedCash() != 60 || e.AvailableCash() != 904 || e.Position("A") != 4 {
		t.Fatalf("首次成交后账务错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 修改为总量 6、限价 7：只替换未成交部分的占用（剩余 2 × 限价 7 = 14），
	// 不重算原先 4 份成交的成本——现金余额 964、持仓 4 不变，可用现金 950。
	mustModify(t, e, bid, 6, 7)
	if e.Cash() != 964 || e.ReservedCash() != 14 || e.AvailableCash() != 950 || e.Position("A") != 4 {
		t.Fatalf("修改后账务错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, ok := e.Order(bid)
	if !ok || o.Qty != 6 || o.Limit != 7 || o.Filled != 4 || o.Remaining() != 2 ||
		o.Status != StatusPartial {
		t.Fatalf("修改后订单应为总量 6、限价 7、已成交 4、剩余 2、部分成交: %+v", o)
	}
	mod := lastModifyRecord(t, e)
	if mod.OrderID != bid || mod.OldQty != 10 || mod.OldLimit != 10 || mod.OldFilled != 4 ||
		mod.Qty != 6 || mod.Limit != 7 || mod.Filled != 4 || mod.Remaining != 2 {
		t.Fatalf("修改记录必须保存修改前后参数与当时已成交量: %+v", mod)
	}

	// 固定重放前的全部记录：接受、成交编号 1、修改，共三条。
	recsBefore := len(e.Records())
	if recsBefore != 3 {
		t.Fatalf("重放前应有 3 条记录（接受+成交+修改），实际 %d", recsBefore)
	}
	recordsBefore := e.Records()
	before := snapshotModifyReplayLedger(e, bid, "A")

	// 按原成交编号、原订单、原合约、原方向、原价格和原数量再次提交：旧成交的
	// 数量 4 超过现在的剩余量 2、价格 9 高于现在的限价 7，仍须返回第一次的结果
	// （本次成交 4 份、累计成交 4 份、当时剩余 6 份、部分成交），不因新参数被拒绝。
	again, err := e.Fill(first)
	if err != nil {
		t.Fatalf("历史成交原样重提不应报错: %v", err)
	}
	if again != res1 {
		t.Fatalf("重放必须返回第一次结果: want %+v got %+v", res1, again)
	}
	if again.Qty != 4 || again.Filled != 4 || again.Remaining != 6 || again.Status != StatusPartial {
		t.Fatalf("重放结果必须保留首次入账时的数量、累计、剩余与状态: %+v", again)
	}

	// 重提不再次扣现金、增加持仓、释放占用或新增事件。
	if got := snapshotModifyReplayLedger(e, bid, "A"); got != before {
		t.Fatalf("重放不得改变资金、持仓或订单:\n之前 %+v\n之后 %+v", before, got)
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("重放不得新增记录，实际 %d 条", len(e.Records()))
	}

	// 订单查询继续显示修改后的总量 6、限价 7、已成交 4、有效剩余 2：
	// 历史返回结果（剩余 6）不能覆盖订单现状。
	o, _ = e.Order(bid)
	if o.Qty != 6 || o.Limit != 7 || o.Filled != 4 || o.Remaining() != 2 ||
		o.Status != StatusPartial {
		t.Fatalf("重放后订单仍应显示修改后的参数: %+v", o)
	}

	// 同一已入账编号把价格改为 7：即使新内容符合当前限价 7，也应报成交编号内容
	// 冲突，只追加一条保留这次回报及冲突原因的拒绝记录，账户和订单不变。
	conflict := Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 7, Qty: 4}
	cres, err := e.Fill(conflict)
	if err == nil {
		t.Fatal("已入账编号改价重提必须报内容冲突")
	}
	for _, s := range []string{"成交编号 1", "已用于不同内容", "价格=9"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("冲突错误 %q 应包含 %q", err.Error(), s)
		}
	}
	if cres != (FillResult{}) {
		t.Fatalf("内容冲突必须返回零值成交结果，实际 %+v", cres)
	}
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("内容冲突只能新增一条拒绝记录，实际新增 %d 条", len(recs)-recsBefore)
	}
	crej := recs[len(recs)-1]
	if crej.Kind != RecordRejected || crej.TradeID != 1 || crej.OrderID != bid ||
		crej.Symbol != "A" || crej.Side != Buy || crej.TradePrice != 7 || crej.Qty != 4 {
		t.Fatalf("冲突拒绝记录必须保留本次提交的回报内容: %+v", crej)
	}
	for _, s := range []string{"成交编号 1", "已用于不同内容"} {
		if !strings.Contains(crej.Reason, s) {
			t.Fatalf("冲突拒绝原因 %q 应包含 %q", crej.Reason, s)
		}
	}
	if !crej.QuoteValid || crej.QuoteSeq != 1 || crej.QuoteMoment != 10 || crej.QuotePrice != 10 {
		t.Fatalf("冲突拒绝记录的报价快照必须取自合约 A: %+v", crej)
	}
	if !crej.MaxPositionValid || crej.MaxPosition != 100 {
		t.Fatalf("冲突拒绝记录的最大持仓量快照必须取自合约 A: %+v", crej)
	}
	if crej.RiskDay != 0 || crej.RiskBaseline != 0 || crej.RiskEquity != 0 ||
		crej.RiskLoss != 0 || crej.RiskLimit != 0 || crej.RiskRestrict {
		t.Fatalf("未开日时冲突拒绝记录不应携带风险快照: %+v", crej)
	}
	if got := snapshotModifyReplayLedger(e, bid, "A"); got != before {
		t.Fatalf("内容冲突不得改变资金、持仓或订单:\n之前 %+v\n之后 %+v", before, got)
	}

	// 尚未入账的编号 2 按价格 9 成交 1 份：价格 9 高于当前限价 7，必须拒绝并返回
	// 零值成交结果，仅留下说明“价格 9 高于限价 7”的拒绝记录；编号 2 不被占用。
	overLimit := Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 9, Qty: 1}
	ores, err := e.Fill(overLimit)
	if err == nil {
		t.Fatal("超过当前限价的新成交必须被拒绝")
	}
	if !strings.Contains(err.Error(), "买入成交价 9 高于限价 7") {
		t.Fatalf("超限拒绝错误应说明价格 9 高于限价 7，实际 %q", err.Error())
	}
	if ores != (FillResult{}) {
		t.Fatalf("超限拒绝必须返回零值成交结果，实际 %+v", ores)
	}
	recs = e.Records()
	if len(recs) != recsBefore+2 {
		t.Fatalf("超限拒绝只能再新增一条拒绝记录，实际共新增 %d 条", len(recs)-recsBefore)
	}
	orej := recs[len(recs)-1]
	if orej.Kind != RecordRejected || orej.TradeID != 2 || orej.OrderID != bid ||
		orej.Symbol != "A" || orej.Side != Buy || orej.TradePrice != 9 || orej.Qty != 1 {
		t.Fatalf("超限拒绝记录必须保留本次提交的回报内容: %+v", orej)
	}
	if !strings.Contains(orej.Reason, "买入成交价 9 高于限价 7") {
		t.Fatalf("超限拒绝原因应说明价格 9 高于限价 7，实际 %q", orej.Reason)
	}
	if got := snapshotModifyReplayLedger(e, bid, "A"); got != before {
		t.Fatalf("超限拒绝不得改变资金、持仓或订单:\n之前 %+v\n之后 %+v", before, got)
	}

	// 把这笔新回报的价格修正为 7 后，沿用编号 2 成功成交 1 份：现金 957、持仓 5、
	// 占用现金 7（剩余 1 × 限价 7），订单累计成交 5、剩余 1，仍为部分成交。
	res2, err := e.Fill(Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 7, Qty: 1})
	if err != nil {
		t.Fatalf("修正价格后沿用编号 2 应成功成交: %v", err)
	}
	if res2.TradeID != 2 || res2.OrderID != bid || res2.Price != 7 || res2.Qty != 1 ||
		res2.Filled != 5 || res2.Remaining != 1 || res2.Status != StatusPartial {
		t.Fatalf("编号 2 成交结果错误: %+v", res2)
	}
	if e.Cash() != 957 || e.ReservedCash() != 7 || e.AvailableCash() != 950 || e.Position("A") != 5 {
		t.Fatalf("编号 2 成交后账务错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, _ = e.Order(bid)
	if o.Qty != 6 || o.Limit != 7 || o.Filled != 5 || o.Remaining() != 1 ||
		o.Status != StatusPartial {
		t.Fatalf("编号 2 成交后订单应为累计成交 5、剩余 1、部分成交: %+v", o)
	}

	// 此前的冲突与超限拒绝记录保留，只新增这笔成功成交的记录。
	recs = e.Records()
	if len(recs) != recsBefore+3 {
		t.Fatalf("最终应只比重放前多三条记录（冲突拒绝+超限拒绝+成交），实际 %d 条",
			len(recs)-recsBefore)
	}
	if recs[recsBefore].Kind != RecordRejected || recs[recsBefore].TradeID != 1 ||
		recs[recsBefore].TradePrice != 7 {
		t.Fatalf("冲突拒绝记录必须保留: %+v", recs[recsBefore])
	}
	if recs[recsBefore+1].Kind != RecordRejected || recs[recsBefore+1].TradeID != 2 ||
		recs[recsBefore+1].TradePrice != 9 {
		t.Fatalf("超限拒绝记录必须保留: %+v", recs[recsBefore+1])
	}
	fill2 := recs[recsBefore+2]
	if fill2.Kind != RecordFilled || fill2.TradeID != 2 || fill2.OrderID != bid ||
		fill2.Symbol != "A" || fill2.Side != Buy || fill2.TradePrice != 7 ||
		fill2.Qty != 1 || fill2.Filled != 5 || fill2.Remaining != 1 {
		t.Fatalf("编号 2 的成交记录错误: %+v", fill2)
	}

	// 原有成交记录与修改记录保持各自发生时的内容，不被后续操作改写。
	for i, want := range recordsBefore {
		if !reflect.DeepEqual(recs[i], want) {
			t.Fatalf("第 %d 条原有记录被改写:\nwant %+v\ngot  %+v", i, want, recs[i])
		}
	}

	// 全部操作之后，历史成交再重放一次仍返回第一次结果，不新增记录。
	again2, err := e.Fill(first)
	if err != nil {
		t.Fatalf("历史成交再次重放不应报错: %v", err)
	}
	if again2 != res1 {
		t.Fatalf("再次重放必须仍返回第一次结果: want %+v got %+v", res1, again2)
	}
	if len(e.Records()) != recsBefore+3 {
		t.Fatalf("再次重放不得新增记录，实际 %d 条", len(e.Records()))
	}
}
