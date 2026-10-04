package book

import (
	"strings"
	"testing"
)

// 本文件为日内亏损保护补充回归保障：调用方在报价缺口尚未补齐时“首次开启交易日”
// 的现有行为。账户可能已经有持仓、部分成交的买单和有效卖单，等待中的报价还可能
// 意味着大幅亏损——开日基准必须以现金余额加持仓按“最新已生效报价”计算的市值
// 确定：未成交买单占用的现金仍是余额的一部分，不能重复扣除；尚在等待补齐的报价
// 不能提前参与估值或触发保护。
//
// 关键构造：初始现金 1000；seq1@时刻10@价格10 已生效；买单总量 15、限价 10，
// 其中 10 股按价格 10 成交（cash=900, pos=10，剩余 5 股占用现金 50）；卖单
// 数量 3、限价 1（占用 3 股可卖数量，可卖数量 7）。随后 seq3@时刻30@价格1
// 因缺少 seq2 只等待——若让它提前估值，净值将只有 910、亏损 90。
//
// 开日（日号 1，亏损上限 90）时基准净值与当前净值都必须是 1000、亏损为 0、
// 保护不触发；开日不产生触线或撤单记录，订单状态、累计成交量与两类占用保留，
// 最新报价停在 seq1、缺口仍在。补齐 seq2@时刻20@价格10 后，seq2 与原先等待的
// seq3 一次依次生效：seq2@10 不触线，seq3@1 使净值跌到 910、亏损恰好 90，
// 保护在刚开启的交易日触发并锚定 seq3 及其时刻，撤销买单剩余 5 股、释放 50
// 现金占用；已入账的现金与 10 股持仓保留，卖单仍有效、可卖数量仍为 7，原成交
// 记录保留成交时的报价快照，不随开日或补齐改写。
//
// 边界：非正日号或负亏损上限开日必须返回错误，原来的风险状态、订单与等待报价
// 全部保留；随后合法开日及补齐仍按上述行为处理。

// startDayGapSetup 构造“缺口等待中首次开日”的共用前置状态：
// 初始现金 1000；A 合约限额 100；seq1@时刻10@价格10 已生效；
// 买单 15 股限价 10 已成交 10 股 @10（cash=900, pos=10，剩余 5 股占用 50）；
// 卖单 3 股限价 1（占用 3 股持仓，可卖 7）；seq3@时刻30@价格1 跨号等待。
// 返回引擎、部分成交买单与有效卖单编号。
func startDayGapSetup(t *testing.T) (e *Engine, buyID, sellID int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	buyID = mustBuy(t, e, "A", 15, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: buyID, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	sellID = mustSell(t, e, "A", 3, 1)

	// seq3 因缺少 seq2 只等待，返回 0 条生效。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 1}, 0)
	if !e.HasGap("A") {
		t.Fatal("缺少 seq2 时 seq3 必须进入等待，报告缺口")
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 1 || q.Moment != 10 || q.Price != 10 {
		t.Fatalf("等待报价不得推进最新报价: %+v ok=%v", q, ok)
	}

	if e.Cash() != 900 || e.ReservedCash() != 50 || e.AvailableCash() != 850 ||
		e.Position("A") != 10 || e.Sellable("A") != 7 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d available=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}
	o, _ := e.Order(buyID)
	if o.Status != StatusPartial || o.Qty != 15 || o.Filled != 10 || o.Remaining() != 5 {
		t.Fatalf("买单应为部分成交、剩余 5 股: %+v", o)
	}
	s, _ := e.Order(sellID)
	if s.Status != StatusPending || s.Qty != 3 || s.Remaining() != 3 {
		t.Fatalf("卖单应保持有效、占用 3 股: %+v", s)
	}
	return e, buyID, sellID
}

// TestRiskStartDayWithPendingGapUsesAppliedQuoteOnly 覆盖主路径：缺口等待中首次
// 开日只按最新已生效报价 seq1 定基准（净值 1000、亏损 0、不触线），开日保留
// 全部订单与占用；补齐 seq2 后 seq2、seq3 依次生效，seq3@1 使亏损恰好达到
// 上限 90 而触发保护。
func TestRiskStartDayWithPendingGapUsesAppliedQuoteOnly(t *testing.T) {
	e, buyID, sellID := startDayGapSetup(t)

	// 开日本身不产生任何记录（不触线、不撤单、不拒绝）。
	recsBeforeStart := len(e.Records())
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatalf("缺口等待中合法开日必须成功: %v", err)
	}
	if len(e.Records()) != recsBeforeStart {
		t.Fatalf("开日不得产生记录，实际新增 %d 条", len(e.Records())-recsBeforeStart)
	}

	// 基准与当前净值都是 1000：现金 900 + 持仓 10×seq1报价10；买单占用的 50
	// 现金属于余额不重复扣除；等待中的 seq3@1 不能让净值提前跌到 910。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 {
		t.Fatalf("交易日应已开启: %+v", st)
	}
	if st.Baseline != 1000 || st.Equity != 1000 || st.Loss != 0 ||
		st.LossLimit != 90 || st.Restricted {
		t.Fatalf("缺口未补齐时基准与净值都应为 1000、亏损 0、保护未触发: %+v", st)
	}

	// 开日保留订单状态、累计成交量和两类占用，最新报价仍停在 seq1，缺口仍在。
	if o, _ := e.Order(buyID); o.Status != StatusPartial || o.Filled != 10 || o.Remaining() != 5 {
		t.Fatalf("开日不得改动部分成交买单: %+v", o)
	}
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("开日不得改动有效卖单: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 50 || e.AvailableCash() != 850 ||
		e.Position("A") != 10 || e.Sellable("A") != 7 {
		t.Fatalf("开日不得改动现金、占用、持仓与可卖数量: cash=%d reserved=%d available=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}
	if q, _ := e.CurrentQuote("A"); q.Seq != 1 || q.Moment != 10 || q.Price != 10 {
		t.Fatalf("开日后最新报价必须停在 seq1@(10,10): %+v", q)
	}
	if !e.HasGap("A") {
		t.Fatal("开日后缺口必须仍在，等待的 seq3 不能被提前生效或丢弃")
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("开日时亏损为 0，不得产生触线记录")
	}

	// 缺口期间新买单仍按既有规则拒绝（与亏损保护无关）；拒绝记录固化开日后的
	// 风险快照（净值 1000、亏损 0、未限制），且不产生触线或撤单记录。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("缺口期间新买单必须拒绝")
	}
	gapReject := e.Records()[len(e.Records())-1]
	if gapReject.Kind != RecordRejected || !strings.Contains(gapReject.Reason, "缺口") {
		t.Fatalf("缺口拒单必须留下缺口原因的拒绝记录: %+v", gapReject)
	}
	if gapReject.QuoteSeq != 1 || gapReject.QuoteMoment != 10 || gapReject.QuotePrice != 10 {
		t.Fatalf("拒绝记录报价快照应停留在 seq1: %+v", gapReject)
	}
	if gapReject.RiskDay != 1 || gapReject.RiskBaseline != 1000 || gapReject.RiskEquity != 1000 ||
		gapReject.RiskLoss != 0 || gapReject.RiskLimit != 90 || gapReject.RiskRestrict {
		t.Fatalf("拒绝记录应固化开日时未触线的风险快照: %+v", gapReject)
	}
	if e.Cash() != 900 || e.ReservedCash() != 50 {
		t.Fatalf("缺口拒单不得改动资金占用: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}

	// 补齐 seq2@(20,10)：链为 seq2@10（净值 1000，不触线）→ seq3@1（净值 910，
	// 亏损恰好 90 达到上限，触线），两条一次依次生效。
	recsBeforeGap := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 10})
	if err != nil {
		t.Fatalf("补齐缺口必须成功: %v", err)
	}
	if n != 2 {
		t.Fatalf("seq2 与等待中的 seq3 应一次依次生效，实际生效 %d 条", n)
	}

	// 保护在刚开启的交易日触发：基准仍为开日时的 1000，当前净值 910、亏损 90。
	st = e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 90 {
		t.Fatalf("开日基准与上限必须保留: %+v", st)
	}
	if !st.Restricted || st.Equity != 910 || st.Loss != 90 {
		t.Fatalf("seq3@1 生效后净值应为 910、亏损恰好 90 并已限制: %+v", st)
	}
	q, _ := e.CurrentQuote("A")
	if q.Seq != 3 || q.Moment != 30 || q.Price != 1 {
		t.Fatalf("最新报价应推进到 seq3@(30,1): %+v", q)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后不得再报告缺口")
	}

	// 撤销买单剩余 5 股并释放 50 现金占用；现金余额与已入账的 10 股持仓保留。
	ob, _ := e.Order(buyID)
	if ob.Status != StatusCanceled || ob.Qty != 15 || ob.Filled != 10 || ob.Remaining() != 0 {
		t.Fatalf("买单应只撤销未成交的 5 股、已成交 10 股保留: %+v", ob)
	}
	if !strings.Contains(ob.Reason, "亏损 90") || !strings.Contains(ob.Reason, "上限 90") {
		t.Fatalf("撤单原因必须固化触线时亏损 90 与上限 90: %q", ob.Reason)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.AvailableCash() != 900 ||
		e.Position("A") != 10 {
		t.Fatalf("撤单只释放占用：余额 900 与持仓 10 不变，占用归零: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 卖单仍有效，可卖数量仍为 7。
	os, _ := e.Order(sellID)
	if os.Status != StatusPending || os.Qty != 3 || os.Limit != 1 || os.Remaining() != 3 {
		t.Fatalf("卖单必须保持有效: %+v", os)
	}
	if e.Sellable("A") != 7 {
		t.Fatalf("卖单占用的 3 股持仓必须保留，可卖数量仍为 7: %d", e.Sellable("A"))
	}

	// 本批只新增“当日首次触线 + 撤销买单”共 2 条记录。
	newRecs := e.Records()[recsBeforeGap:]
	if len(newRecs) != 2 {
		t.Fatalf("补齐应只新增 2 条记录（1 触线 + 1 撤单），实际 %d 条", len(newRecs))
	}
	trig := newRecs[0]
	if trig.Kind != RecordRiskTriggered {
		t.Fatalf("第一条新增记录必须是触线记录: %+v", trig)
	}
	if trig.RiskTrigger != RiskTriggerQuote || trig.RiskRefSeq != 3 || trig.RiskRefMoment != 30 {
		t.Fatalf("触线必须由报价引发并关联 seq3 及其时刻 30: trigger=%s ref=%d moment=%d",
			trig.RiskTrigger, trig.RiskRefSeq, trig.RiskRefMoment)
	}
	if trig.RiskDay != 1 || trig.RiskBaseline != 1000 || trig.RiskEquity != 910 ||
		trig.RiskLoss != 90 || trig.RiskLimit != 90 || !trig.RiskRestrict {
		t.Fatalf("触线快照必须固化 seq3@1 下的净值 910、亏损 90: %+v", trig)
	}
	if len(trig.RiskQuoteRefs) != 1 ||
		trig.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 3, Moment: 30, Price: 1}) {
		t.Fatalf("触线估值报价定位必须锚定 seq3@(30,1): %+v", trig.RiskQuoteRefs)
	}
	cr := newRecs[1]
	if cr.Kind != RecordCanceled || cr.OrderID != buyID || cr.Filled != 10 || cr.Remaining != 5 {
		t.Fatalf("第二条记录必须是撤销买单剩余 5 股、已成交 10 股: %+v", cr)
	}
	if cr.QuoteSeq != 3 || cr.QuoteMoment != 30 || cr.QuotePrice != 1 {
		t.Fatalf("撤单报价快照应锚定触线时的 seq3@(30,1): %+v", cr)
	}
	if cr.RiskEquity != 910 || cr.RiskLoss != 90 || cr.RiskLimit != 90 || !cr.RiskRestrict {
		t.Fatalf("撤单记录应携带触线时风险快照: %+v", cr)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 原成交记录保留成交时的报价（seq1@(10,10)）与成交价，不随开日或补齐改写；
	// 成交发生在开日前，也不得事后补写风险快照。
	var fillRec Record
	for _, r := range e.Records() {
		if r.Kind == RecordFilled && r.TradeID == 1 {
			fillRec = r
		}
	}
	if fillRec.TradeID != 1 {
		t.Fatal("必须能找到成交编号 1 的原成交记录")
	}
	if !fillRec.QuoteValid || fillRec.QuoteSeq != 1 || fillRec.QuoteMoment != 10 || fillRec.QuotePrice != 10 {
		t.Fatalf("原成交记录的报价快照必须保留成交时的 seq1@(10,10): %+v", fillRec)
	}
	if fillRec.TradePrice != 10 || fillRec.Qty != 10 || fillRec.Filled != 10 {
		t.Fatalf("原成交记录的成交内容必须保留: %+v", fillRec)
	}
	if fillRec.RiskDay != 0 || fillRec.RiskRestrict {
		t.Fatalf("开日前的成交记录不得被事后补写风险快照: %+v", fillRec)
	}

	// 限制保留：补齐缺口后新买单仍被拒绝。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("保护触发后新买单必须拒绝")
	}
}

// TestRiskStartDayInvalidParamsPreserveGapAndOrders 覆盖开日参数失败边界：
// 非正日号、负亏损上限都必须返回错误且不留记录，未开日状态、订单、两类占用与
// 等待报价原样保留；修正参数后合法开日与补齐仍按主路径行为处理。
func TestRiskStartDayInvalidParamsPreserveGapAndOrders(t *testing.T) {
	e, buyID, sellID := startDayGapSetup(t)

	for _, tc := range []struct {
		name      string
		day       int64
		lossLimit int64
	}{
		{"日号为零", 0, 90},
		{"日号为负", -3, 90},
		{"亏损上限为负", 1, -1},
		{"日号与上限均非法", -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recsBefore := len(e.Records())
			if err := e.StartTradingDay(tc.day, tc.lossLimit); err == nil {
				t.Fatalf("开日(%d, %d) 必须返回错误", tc.day, tc.lossLimit)
			}
			if len(e.Records()) != recsBefore {
				t.Fatalf("非法开日不得追加记录，实际新增 %d 条", len(e.Records())-recsBefore)
			}
			if e.RiskStatus().Open {
				t.Fatal("非法开日后必须仍处于未开日状态")
			}
			if o, _ := e.Order(buyID); o.Status != StatusPartial || o.Filled != 10 || o.Remaining() != 5 {
				t.Fatalf("非法开日不得改动买单: %+v", o)
			}
			if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 3 {
				t.Fatalf("非法开日不得改动卖单: %+v", o)
			}
			if e.Cash() != 900 || e.ReservedCash() != 50 || e.Position("A") != 10 ||
				e.Sellable("A") != 7 {
				t.Fatalf("非法开日不得改动资金与占用: cash=%d reserved=%d pos=%d sellable=%d",
					e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
			}
			if q, _ := e.CurrentQuote("A"); q.Seq != 1 || q.Price != 10 {
				t.Fatalf("非法开日后最新报价必须停在 seq1@10: %+v", q)
			}
			if !e.HasGap("A") {
				t.Fatal("非法开日后等待中的 seq3 必须继续等待")
			}
			if countRiskRecords(e) != 0 {
				t.Fatal("非法开日不得产生触线记录")
			}
		})
	}

	// 修正参数后合法开日：仍按最新已生效报价 seq1 定基准，等待报价不参与。
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatalf("合法开日必须成功: %v", err)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.Equity != 1000 ||
		st.Loss != 0 || st.LossLimit != 90 || st.Restricted {
		t.Fatalf("非法尝试后合法开日仍应得到基准 1000、亏损 0、未触发: %+v", st)
	}
	if !e.HasGap("A") {
		t.Fatal("合法开日后缺口仍必须保留")
	}

	// 补齐后行为与主路径一致：seq2、seq3 依次生效，seq3 触发保护并撤单。
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 10})
	if err != nil || n != 2 {
		t.Fatalf("补齐应使 seq2/seq3 依次生效: n=%d err=%v", n, err)
	}
	st = e.RiskStatus()
	if !st.Restricted || st.Equity != 910 || st.Loss != 90 {
		t.Fatalf("补齐后应在 seq3 触线：净值 910、亏损 90: %+v", st)
	}
	if o, _ := e.Order(buyID); o.Status != StatusCanceled || o.Filled != 10 || o.Remaining() != 0 {
		t.Fatalf("买单剩余 5 股必须被保护撤销、已成交 10 股保留: %+v", o)
	}
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("卖单必须保持有效: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.Position("A") != 10 ||
		e.Sellable("A") != 7 {
		t.Fatalf("撤单只释放 50 占用：余额 900、持仓 10、卖单占用与可卖 7 不变: "+
			"cash=%d reserved=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("合法交易日只能有一条触线记录，非法尝试不得占用")
	}
}
