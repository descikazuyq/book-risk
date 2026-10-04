package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// 本文件为“日内亏损保护 + 账户总持仓金额上限”同时启用时的补齐行情缺口补充
// 回归保障：同一批连续报价中，亏损保护在中间报价撤销的买单，其剩余量不得再
// 参与链中后续报价的账户金额计算；但排除规则不得扩大为忽略真实持仓。
//
// 关键构造：账户已开日、尚未限制买入，已有持仓和一笔部分成交的有效买单，
// 两项限额在提交前都允许保留该订单；后续序号的报价先到并等待，补齐缺失报价
// 后，同一批连续报价先出现让日内亏损触线的价格，再出现大幅回升的价格。回升
// 报价下真实持仓市值与净值都能用 int64 表示，但若把已撤买单的剩余量继续计入
// 金额，占用乘积必然溢出 int64——已撤剩余量（18 股）大于持仓（12 股），
// 使得“18×回升价”溢出而“12×回升价”可表示，专用于识别误计入。
//
// 成功路径要求：补齐成功并返回实际生效条数，最新报价推进到末条、缺口消失；
// 中间报价触线撤销该买单全部未成交部分并释放现金占用，已成交数量、持仓与
// 现金余额保持原值；末条回升后日内限制保留，金额查询按末条报价计算真实持仓、
// 买单占用为零（既不为避开溢出漏算持仓，也不沿用触线时的低价）；触线与撤销
// 记录锚定真正触线的中间报价，撤单原因是日内亏损保护，无金额溢出拒绝记录。
//
// 失败路径要求：末条报价使仍保留的持仓市值或净值超出 int64 时，整批返回包装
// ErrInt64Overflow 的错误与零条生效结果；报价、订单、资金占用与日内限制维持
// 提交前状态，原先等待的报价继续等待，只新增一条说明溢出的拒绝记录，不留下
// 本批中间报价拟产生的触线或撤销记录。

// riskAmountGapSetup 构造共用前置状态：
// 初始现金 1000，A 合约 10 股 @10 已成交入账（cash=900, pos=10, seq1@10）；
// 开日（日号 1，亏损上限 50，基准 1000）后，买单 20 股限价 8 部分成交 2 股 @8
// （cash=884, pos=12, 剩余 18 股占用现金 144）；再设账户总持仓金额上限 300
// （持仓 12×10=120 + 买单剩余 18×max(8,10)=180，合计恰为 300，设置时不撤单）。
//
// 此时亏损 0 未触线、金额合计恰满，两项限额都允许保留该部分成交买单。
func riskAmountGapSetup(t *testing.T) (e *Engine, bPartial int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b0 := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b0, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 50); err != nil {
		t.Fatal(err)
	}
	bPartial = mustBuy(t, e, "A", 20, 8)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bPartial, Symbol: "A", Side: Buy, Price: 8, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(300); err != nil {
		t.Fatal(err)
	}

	if e.Cash() != 884 || e.ReservedCash() != 144 || e.Position("A") != 12 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	st := e.RiskStatus()
	if !st.Open || st.Restricted || st.Equity != 1004 || st.Loss != 0 {
		t.Fatalf("提交前必须未触线: %+v", st)
	}
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Limit != 300 || am.Holding != 120 || am.BuyReserved != 180 || am.Total != 300 {
		t.Fatalf("提交前金额状态必须恰好允许保留该买单: %+v", am)
	}
	return e, bPartial
}

// TestRiskAmountGapFillLossCanceledBuyExcludedFromLaterQuote 覆盖成功路径：
// seq3@5 使亏损 56 达到上限 50（链中触线，撤销买单剩余 18 股）；seq4 大幅
// 回升，12×p4 与现金加持仓净值都可表示，但 18×p4 必然溢出。若实现错误地把
// 已撤剩余量计入 seq4 的金额计算，整批会被误判溢出而回滚；正确实现必须成功。
func TestRiskAmountGapFillLossCanceledBuyExcludedFromLaterQuote(t *testing.T) {
	e, bPartial := riskAmountGapSetup(t)

	// p4 = MaxInt64/18 + 1：已撤的 18 股若被计入则 18×p4 溢出；
	// 真实持仓 12×p4 与净值 884+12×p4 都可表示。
	p4 := int64(math.MaxInt64/18 + 1)
	wantHolding := 12 * p4
	wantEquity := 884 + wantHolding
	if p4 <= math.MaxInt64/18 { // 防御性自检：构造必须保证 18×p4 溢出
		t.Fatal("测试构造错误：18×p4 必须溢出 int64")
	}

	// 后续序号先到并等待：seq3 触线价、seq4 回升价。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 5}, 0)
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: p4}, 0)
	if !e.HasGap("A") {
		t.Fatal("seq3/seq4 跨号必须进入等待")
	}

	// 等待中的报价不得提前改变风险状态、金额状态、订单与占用。
	if st := e.RiskStatus(); st.Restricted || st.Equity != 1004 || st.Loss != 0 {
		t.Fatalf("等待报价不得提前参与估值或触线: %+v", st)
	}
	if am := e.PositionAmountStatus(); am.Holding != 120 || am.BuyReserved != 180 || am.Total != 300 {
		t.Fatalf("等待报价不得提前参与金额计算: %+v", am)
	}
	if o, _ := e.Order(bPartial); o.Status != StatusPartial || o.Remaining() != 18 {
		t.Fatalf("等待期间不得提前撤单: %+v", o)
	}
	if e.ReservedCash() != 144 {
		t.Fatalf("等待期间现金占用不得变化: %d", e.ReservedCash())
	}

	// 补齐缺失的 seq2@9：链为 seq2@9（亏损 8，不触线）→ seq3@5（亏损 56，
	// 触线撤单）→ seq4@p4（回升，亏损归零），三条全部生效。
	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 9})
	if err != nil {
		t.Fatalf("已撤买单不得被回升报价计入致整批报金额溢出: %v", err)
	}
	if n != 3 {
		t.Fatalf("本次应新生效 3 条报价（seq2/3/4），实际 %d", n)
	}

	// 即使批末回升，日内限制必须保留；净值按末条报价计算、亏损归零。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 50 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted {
		t.Fatal("中间报价触线后，即使批末回升，限制也必须保留")
	}
	if st.Equity != wantEquity || st.Loss != 0 {
		t.Fatalf("净值必须按末条报价 seq4 估值: %+v want equity=%d", st, wantEquity)
	}

	// 中间报价触线撤销该买单全部未成交的 18 股，已成交 2 股保留。
	o, _ := e.Order(bPartial)
	if o.Status != StatusCanceled || o.Filled != 2 || o.Remaining() != 0 {
		t.Fatalf("部分成交买单只撤销未成交的 18 股、已成交 2 股保留: %+v", o)
	}
	if !strings.Contains(o.Reason, "日内亏损 56") || !strings.Contains(o.Reason, "上限 50") ||
		!strings.Contains(o.Reason, "风险保护") {
		t.Fatalf("撤单原因必须是日内亏损保护并固化触线时的亏损 56 与上限 50: %q", o.Reason)
	}

	// 释放剩余量对应的现金占用 144；已成交入账的现金与持仓保持原值。
	if e.ReservedCash() != 0 {
		t.Fatalf("被撤买单剩余现金占用必须全部释放: %d", e.ReservedCash())
	}
	if e.Cash() != 884 || e.Position("A") != 12 {
		t.Fatalf("已成交入账的现金与持仓不得变化: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}

	// 最新报价推进到末条，缺口消失。
	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 4 || q.Price != p4 {
		t.Fatalf("最新报价应推进到末条 seq4@p4: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("链全部生效后不得再报告缺口")
	}

	// 金额查询按末条报价计算真实持仓：Holding=12×p4，买单占用为零。
	// 不能为避开溢出漏算持仓（Holding=0），也不能沿用触线时的低价（12×5=60）。
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Limit != 300 {
		t.Fatalf("金额上限设置应保持: %+v", am)
	}
	if am.Holding != wantHolding || am.BuyReserved != 0 || am.Total != wantHolding {
		t.Fatalf("末条报价下应只剩真实持仓市值、买单占用为零: %+v want holding=%d", am, wantHolding)
	}

	// 本批只新增“当日首次触线 + 实际撤单”共 2 条记录，无任何拒绝记录。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 2 {
		t.Fatalf("应只新增 2 条记录（1 触线 + 1 撤单），实际 %d 条", len(newRecs))
	}
	trig := newRecs[0]
	if trig.Kind != RecordRiskTriggered {
		t.Fatalf("第一条新增记录必须是触线记录: %+v", trig)
	}
	if trig.RiskTrigger != RiskTriggerQuote || trig.RiskRefSeq != 3 || trig.RiskRefMoment != 3 {
		t.Fatalf("触线来源与报价定位必须固化在真正触线的 seq3@时刻3: trigger=%s ref=%d moment=%d",
			trig.RiskTrigger, trig.RiskRefSeq, trig.RiskRefMoment)
	}
	if trig.RiskDay != 1 || trig.RiskBaseline != 1000 || trig.RiskEquity != 944 ||
		trig.RiskLoss != 56 || trig.RiskLimit != 50 || !trig.RiskRestrict {
		t.Fatalf("触线快照必须固化中间价下的亏损: %+v", trig)
	}
	if len(trig.RiskQuoteRefs) != 1 ||
		trig.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 3, Moment: 3, Price: 5}) {
		t.Fatalf("参与估值的报价定位必须锚定 seq3@5: %+v", trig.RiskQuoteRefs)
	}

	cr := newRecs[1]
	if cr.Kind != RecordCanceled || cr.OrderID != bPartial || cr.Filled != 2 || cr.Remaining != 18 {
		t.Fatalf("撤单记录必须标明取消剩余 18 股、已成交 2 股: %+v", cr)
	}
	if !strings.Contains(cr.Reason, "日内亏损 56") {
		t.Fatalf("撤单记录原因必须来自日内亏损保护: %q", cr.Reason)
	}
	if cr.QuoteSeq != 3 || cr.QuotePrice != 5 || cr.QuoteMoment != 3 {
		t.Fatalf("撤单报价快照应锚定触线时的 seq3@5: %+v", cr)
	}
	if cr.RiskDay != 1 || cr.RiskEquity != 944 || cr.RiskLoss != 56 ||
		cr.RiskLimit != 50 || !cr.RiskRestrict {
		t.Fatalf("撤单记录应携带触线时风险快照: %+v", cr)
	}
	if cr.AmtEnabled {
		t.Fatalf("撤单由亏损保护引发，不得记为金额上限撤单: %+v", cr)
	}
	for _, r := range newRecs {
		if r.Kind == RecordRejected {
			t.Fatalf("成功补齐不得新增金额溢出等拒绝记录: %+v", r)
		}
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 限制保留：新买单继续被拒（与本批事件记录无关，放在记录断言之后）。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("限制保留期间新买单必须拒绝")
	}
}

// TestRiskAmountGapFillLaterHoldingOverflowRollsBack 覆盖边界之一：链中 seq3@5
// 本会触线撤单，但链末 seq4 使仍保留的持仓市值 12×p4 超出 int64。撤单排除
// 规则不得扩大为忽略持仓——整批必须回滚：0 条生效、不触线、不撤单，订单、
// 资金占用与日内限制保持提交前状态，等待报价继续等待，只新增一条拒绝记录。
func TestRiskAmountGapFillLaterHoldingOverflowRollsBack(t *testing.T) {
	e, bPartial := riskAmountGapSetup(t)

	p4 := int64(math.MaxInt64 / 8) // 12×p4 必然溢出 int64
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 5}, 0)
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: p4}, 0)

	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 9})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("链末持仓市值溢出必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if n != 0 {
		t.Fatalf("整批失败时生效条数必须为零，实际 %d", n)
	}

	// 最新报价、开日基准、亏损上限与未限制状态全部保留。
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 10 {
		t.Fatalf("回滚后最新报价应停留在 seq1@10: %+v", q)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 50 {
		t.Fatalf("开日状态必须保留: %+v", st)
	}
	if st.Restricted || st.Equity != 1004 || st.Loss != 0 {
		t.Fatalf("拟触发的触线不得留下，净值保持提交前估值: %+v", st)
	}

	// 链中本拟触发的撤单不得改变订单、资金占用与持仓。
	if o, _ := e.Order(bPartial); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 18 {
		t.Fatalf("部分成交买单必须保持提交前状态: %+v", o)
	}
	if e.ReservedCash() != 144 || e.Cash() != 884 || e.Position("A") != 12 {
		t.Fatalf("回滚后资金与占用必须保持提交前值: reserved=%d cash=%d pos=%d",
			e.ReservedCash(), e.Cash(), e.Position("A"))
	}
	if am := e.PositionAmountStatus(); am.Holding != 120 || am.BuyReserved != 180 || am.Total != 300 {
		t.Fatalf("回滚后金额状态应恢复提交前值: %+v", am)
	}

	// 原先等待的 seq3/seq4 仍然等待，不能被当作已处理报价丢弃。
	if !e.HasGap("A") {
		t.Fatal("回滚后等待中的报价必须继续等待")
	}

	// 只新增一条说明估值溢出的拒绝记录，不得留下触线或撤单记录。
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("整批失败只能新增一条记录，实际 %d 条", len(recs)-recsBefore)
	}
	rj := recs[recsBefore]
	if rj.Kind != RecordRejected {
		t.Fatalf("唯一新增记录必须是拒绝记录: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "序号 4") || !strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因必须指明链中无法估值的 seq4 与 int64 溢出: %q", rj.Reason)
	}
	if rj.QuoteSeq != 1 || rj.QuotePrice != 10 {
		t.Fatalf("拒绝记录的报价快照应停留在 seq1@10: %+v", rj)
	}
	if rj.RiskDay != 1 || rj.RiskEquity != 1004 || rj.RiskLoss != 0 || rj.RiskRestrict {
		t.Fatalf("拒绝记录应携带提交前的风险快照: %+v", rj)
	}
	for _, r := range recs[recsBefore:] {
		if r.Kind == RecordRiskTriggered || r.Kind == RecordCanceled {
			t.Fatalf("失败提交不得留下触线或撤单记录: %+v", r)
		}
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("整批失败后不得存在任何触线记录")
	}

	// 等待报价同内容重复提交仍是无操作；再次补齐会重新组链并因同一溢出失败。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 5}, 0)
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: p4}, 0)
	if n2, err2 := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 9}); !errors.Is(err2, ErrInt64Overflow) || n2 != 0 {
		t.Fatalf("等待报价保留时，重新补齐必须仍可识别同一溢出: n=%d err=%v", n2, err2)
	}
}

// TestRiskAmountGapFillLaterEquityOverflowRollsBack 覆盖边界之二：链末报价下
// 持仓市值 12×p4 本身仍可用 int64 表示，但现金余额加持仓市值的净值溢出。
// 这同样必须识别为估值溢出并整批回滚，且不得把撤单排除规则扩大为忽略持仓。
func TestRiskAmountGapFillLaterEquityOverflowRollsBack(t *testing.T) {
	e, bPartial := riskAmountGapSetup(t)

	p4 := int64(math.MaxInt64 / 12) // 12×p4 可表示，884+12×p4 溢出
	if 12*p4 > math.MaxInt64 {
		t.Fatal("测试构造错误：12×p4 必须可表示")
	}
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 5}, 0)
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: p4}, 0)

	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 9})
	if !errors.Is(err, ErrInt64Overflow) || n != 0 {
		t.Fatalf("现金加持仓净值溢出必须整批失败且 0 条生效: n=%d err=%v", n, err)
	}

	// 报价、订单、资金占用、金额状态与日内限制全部保持提交前状态。
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 10 || !e.HasGap("A") {
		t.Fatalf("回滚后最新报价停留 seq1 且 seq3/seq4 继续等待: q=%+v gap=%v", q, e.HasGap("A"))
	}
	st := e.RiskStatus()
	if st.Restricted || st.Equity != 1004 || st.Loss != 0 || st.Baseline != 1000 || st.LossLimit != 50 {
		t.Fatalf("拟触线不得留下，风险状态保持提交前: %+v", st)
	}
	if o, _ := e.Order(bPartial); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 18 {
		t.Fatalf("部分成交买单必须保持提交前状态: %+v", o)
	}
	if e.ReservedCash() != 144 || e.Cash() != 884 || e.Position("A") != 12 {
		t.Fatalf("资金与占用必须保持提交前值: reserved=%d cash=%d pos=%d",
			e.ReservedCash(), e.Cash(), e.Position("A"))
	}
	if am := e.PositionAmountStatus(); am.Holding != 120 || am.BuyReserved != 180 || am.Total != 300 {
		t.Fatalf("金额状态应恢复提交前值: %+v", am)
	}

	// 只新增一条说明估值溢出的拒绝记录，不得留下触线或撤单记录。
	recs := e.Records()
	if len(recs) != recsBefore+1 || recs[recsBefore].Kind != RecordRejected {
		t.Fatalf("只能新增一条拒绝记录，实际 %d 条", len(recs)-recsBefore)
	}
	rj := recs[recsBefore]
	if !strings.Contains(rj.Reason, "序号 4") || !strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因必须指明 seq4 的 int64 估值溢出: %q", rj.Reason)
	}
	for _, r := range recs[recsBefore:] {
		if r.Kind == RecordRiskTriggered || r.Kind == RecordCanceled {
			t.Fatalf("失败提交不得留下触线或撤单记录: %+v", r)
		}
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("整批失败后不得存在触线记录")
	}
}
