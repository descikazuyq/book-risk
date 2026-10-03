package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// 本文件为日内亏损保护补充回归保障：调用方补齐行情缺口、一次使多条报价连续
// 生效时，链中判断必须按“前一条报价实际产生后的状态”逐条承接。
//
// 关键构造：账户已开日、尚未触线并持有合约仓位，同时保留待成交买单、部分
// 成交买单与有效卖单；后续序号的报价先到并等待，再提交缺失报价，使同一批
// 报价中先出现触线价格，随后出现恢复价格（成功路径）或无法完成估值的天价
// （溢出回滚路径）。
//
// 成功路径要求：即使批末价格恢复、当前亏损回到上限以下，中间报价一旦触线，
// 日内限制整日保留；全部仍有效买单按编号从大到小撤销，部分成交买单只释放
// 未成交部分，已入账现金与持仓不变，卖单及其可卖数量占用不受影响；最新报价
// 推进到批末，当前净值按最后生效报价计算；触线记录固化在真正触线的中间时点。
//
// 失败路径要求：链末报价使持仓市值或“现金 + 市值”净值溢出 int64 时，整批
// 返回包装 ErrInt64Overflow 的错误与零条生效结果；最新报价、当日基准、亏损
// 上限与限制状态全部保留，链中本拟触发的撤单不得改变任何订单、现金占用、
// 持仓或卖单占用；原先等待的报价继续等待；只新增一条说明估值溢出的拒绝记录。

// riskGapSetup 构造共用前置状态：
// 初始现金 1000，A 合约 10 股 @10 已成交入账（cash=900, pos=10, seq1@10）。
// 开日（日号 1，亏损上限 50，基准 1000）后再：
//   - bPending：A 待成交买单 3 股限价 9（现金占用 27，编号在后）
//   - bPartial：A 买单 4 股限价 8 并部分成交 2 股 @8
//     （cash=884, pos=12, 剩余 2 股占用现金 16）
//   - sellID：A 有效卖单 2 股限价 5（占用可卖数量 2）
//
// 返回引擎与三个订单编号；此时现金占用合计 43，可卖数量 10。
func riskGapSetup(t *testing.T) (e *Engine, bPending, bPartial, sellID int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bid := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 50); err != nil {
		t.Fatal(err)
	}
	bPending = mustBuy(t, e, "A", 3, 9)
	bPartial = mustBuy(t, e, "A", 4, 8)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bPartial, Symbol: "A", Side: Buy, Price: 8, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	sellID = mustSell(t, e, "A", 2, 5)

	if e.Cash() != 884 || e.ReservedCash() != 43 || e.Position("A") != 12 || e.Sellable("A") != 10 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}
	return e, bPending, bPartial, sellID
}

// TestRiskGapFillTriggerMidBatchThenRecover 覆盖成功路径：
// seq3@5 使亏损 56 达到上限 50（链中触线），seq4@10 使净值恢复到 1004、
// 亏损归零；补齐 seq2@9 后三条报价一次生效。限制必须保留，触线锚定 seq3，
// 撤单按编号降序，批末净值用 seq4 估值。
func TestRiskGapFillTriggerMidBatchThenRecover(t *testing.T) {
	e, bPending, bPartial, sellID := riskGapSetup(t)

	// 后续序号先到并等待：seq3 触线价、seq4 恢复价。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 5}, 0)
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: 10}, 0)
	if !e.HasGap("A") {
		t.Fatal("seq3/seq4 跨号必须进入等待")
	}

	// 等待中的报价不得提前改变净值、风险状态或订单。
	st := e.RiskStatus()
	if st.Restricted || st.Equity != 1004 || st.Loss != 0 {
		t.Fatalf("等待报价不得提前参与估值或改变风险状态: %+v", st)
	}
	if o, _ := e.Order(bPending); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("等待期间不得提前撤单: %+v", o)
	}
	if o, _ := e.Order(bPartial); o.Status != StatusPartial || o.Remaining() != 2 {
		t.Fatalf("等待期间部分成交买单必须保持: %+v", o)
	}
	if e.ReservedCash() != 43 || e.Sellable("A") != 10 {
		t.Fatalf("等待期间占用不得变化: reserved=%d sellable=%d", e.ReservedCash(), e.Sellable("A"))
	}

	// 补齐缺失的 seq2@9：链为 seq2@9（亏损 8，不触线）→ seq3@5（亏损 56，
	// 触线）→ seq4@10（恢复，亏损 0），三条全部生效。
	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 9})
	if err != nil {
		t.Fatalf("可完成估值的补齐必须成功: %v", err)
	}
	if n != 3 {
		t.Fatalf("本次应新生效 3 条报价（seq2/3/4），实际 %d", n)
	}

	// 即使批末已恢复，限制必须保留。
	st = e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 50 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted {
		t.Fatal("中间报价触线后，即使批末价格恢复，限制也必须保留")
	}
	// 当前净值必须使用最后生效报价 seq4@10：884 + 12×10 = 1004，亏损归零，
	// 不能因为保留限制而停在触线时的 944。
	if st.Equity != 1004 || st.Loss != 0 {
		t.Fatalf("查询净值必须按批末报价估值: %+v", st)
	}

	// 全部仍有效买单按编号从大到小撤销（bPartial 编号更大，先撤）。
	oP, _ := e.Order(bPartial)
	if oP.Status != StatusCanceled || oP.Filled != 2 || oP.Remaining() != 0 {
		t.Fatalf("部分成交买单只撤销未成交的 2 股、已成交 2 股保留: %+v", oP)
	}
	if !strings.Contains(oP.Reason, "亏损 56") || !strings.Contains(oP.Reason, "上限 50") {
		t.Fatalf("撤单原因必须固化触线时的亏损 56 与上限 50，不能按批末恢复后的亏损 0 改写: %q", oP.Reason)
	}
	oB, _ := e.Order(bPending)
	if oB.Status != StatusCanceled || oB.Filled != 0 || oB.Remaining() != 0 {
		t.Fatalf("待成交买单整单撤销（订单剩余量归零，被取消的 3 股固化在撤单记录）: %+v", oB)
	}
	if !strings.Contains(oB.Reason, "亏损 56") {
		t.Fatalf("待成交买单撤单原因同样固化触线时亏损: %q", oB.Reason)
	}

	// 部分成交只释放未成交部分的现金与数量占用：27+16=43 全部释放，
	// 已入账现金 884 与持仓 12 不变。
	if e.ReservedCash() != 0 {
		t.Fatalf("被撤买单剩余现金占用必须全部释放: %d", e.ReservedCash())
	}
	if e.Cash() != 884 || e.Position("A") != 12 {
		t.Fatalf("已成交入账的现金与持仓不得变化: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}

	// 卖单及其可卖数量占用不受影响。
	oSell, _ := e.Order(sellID)
	if oSell.Status != StatusPending || oSell.Remaining() != 2 {
		t.Fatalf("卖单必须保持有效: %+v", oSell)
	}
	if e.Sellable("A") != 10 {
		t.Fatalf("卖单占用的可卖数量必须保留: %d", e.Sellable("A"))
	}

	// 最新报价推进到批末，缺口消失。
	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 4 || q.Price != 10 {
		t.Fatalf("最新报价应推进到批末 seq4@10: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("链全部生效后不得再报告缺口")
	}

	// 本批只新增“当日首次触线 + 实际撤单（2 条）”共 3 条记录，无拒绝记录；
	// 顺序为触线、撤 bPartial（编号大）、撤 bPending。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("应只新增 3 条记录（1 触线 + 2 撤单），实际 %d 条", len(newRecs))
	}
	trig := newRecs[0]
	if trig.Kind != RecordRiskTriggered {
		t.Fatalf("第一条新增记录必须是触线记录: %+v", trig)
	}
	if trig.RiskTrigger != RiskTriggerQuote || trig.RiskRefSeq != 3 || trig.RiskRefMoment != 3 {
		t.Fatalf("触线来源与报价定位必须固化在 seq3@时刻3: trigger=%s ref=%d moment=%d",
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

	c1, c2 := newRecs[1], newRecs[2]
	if c1.Kind != RecordCanceled || c1.OrderID != bPartial || c1.Filled != 2 || c1.Remaining != 2 {
		t.Fatalf("第一条撤单必须是编号较大的部分成交买单，只取消剩余 2 股: %+v", c1)
	}
	if c2.Kind != RecordCanceled || c2.OrderID != bPending || c2.Filled != 0 || c2.Remaining != 3 {
		t.Fatalf("第二条撤单必须是待成交买单，取消 3 股: %+v", c2)
	}
	for _, cr := range [2]Record{c1, c2} {
		if cr.QuoteSeq != 3 || cr.QuotePrice != 5 || cr.QuoteMoment != 3 {
			t.Fatalf("撤单报价快照应锚定触线时的 seq3@5: %+v", cr)
		}
		// 撤单记录的风险净值同样取触线价（现金占用是余额的一部分，撤单不改净值）。
		if cr.RiskDay != 1 || cr.RiskEquity != 944 || cr.RiskLoss != 56 ||
			cr.RiskLimit != 50 || !cr.RiskRestrict {
			t.Fatalf("撤单记录应携带触线时风险快照: %+v", cr)
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

// TestRiskGapFillLaterHoldingOverflowRollsBackMidBatchTrigger 覆盖失败路径之一：
// 链中 seq3@5 本会触线并撤销两笔买单，但链末 seq4 天价使持仓 12×p4 无法用
// int64 表示（持仓市值溢出）。整批必须回滚：0 条生效、不触线、不撤单，
// 订单、现金/数量占用、卖单占用全部保持提交前值，等待报价继续等待。
func TestRiskGapFillLaterHoldingOverflowRollsBackMidBatchTrigger(t *testing.T) {
	e, bPending, bPartial, sellID := riskGapSetup(t)

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

	// 最新报价、当日基准、亏损上限与限制状态全部保留（未触线）。
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

	// 链中本拟触发的撤单不得改变任何订单。
	if o, _ := e.Order(bPending); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 3 {
		t.Fatalf("待成交买单必须保持待成交、剩余 3: %+v", o)
	}
	if o, _ := e.Order(bPartial); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 2 {
		t.Fatalf("部分成交买单必须保持部分成交、剩余 2: %+v", o)
	}
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("卖单必须保持有效: %+v", o)
	}

	// 现金占用、已入账现金、持仓与卖单的可卖数量占用全部保持提交前值。
	if e.ReservedCash() != 43 || e.Cash() != 884 || e.Position("A") != 12 || e.Sellable("A") != 10 {
		t.Fatalf("回滚后资金与占用必须保持提交前值: reserved=%d cash=%d pos=%d sellable=%d",
			e.ReservedCash(), e.Cash(), e.Position("A"), e.Sellable("A"))
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
	// 拒绝记录固化提交前（seq1@10）的报价与风险快照。
	if rj.QuoteSeq != 1 || rj.QuotePrice != 10 {
		t.Fatalf("拒绝记录的报价快照应停留在 seq1@10: %+v", rj)
	}
	if rj.RiskDay != 1 || rj.RiskBaseline != 1000 || rj.RiskEquity != 1004 ||
		rj.RiskLoss != 0 || rj.RiskLimit != 50 || rj.RiskRestrict {
		t.Fatalf("拒绝记录应携带提交前的风险快照: %+v", rj)
	}
	for _, r := range recs[recsBefore:] {
		if r.Kind == RecordRiskTriggered {
			t.Fatalf("失败提交不得留下触线记录: %+v", r)
		}
		if r.Kind == RecordCanceled {
			t.Fatalf("失败提交不得留下撤单记录: %+v", r)
		}
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("整批失败后不得存在任何触线记录")
	}

	// 等待报价同内容重复提交仍是无操作（继续等待）；缺失的 seq2 未被吞掉，
	// 再次补齐会重新组链并因同一溢出失败。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 5}, 0)
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: p4}, 0)
	if n2, err2 := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 9}); !errors.Is(err2, ErrInt64Overflow) || n2 != 0 {
		t.Fatalf("等待报价保留时，重新补齐必须仍可识别同一溢出: n=%d err=%v", n2, err2)
	}
}

// TestRiskGapFillLaterCashPlusHoldingOverflowRollsBack 覆盖失败路径之二：
// 链末报价下持仓市值本身仍可用 int64 表示（1×MaxInt64），但现金余额加该市值
// 后净值溢出。这同样必须识别为估值溢出并整批回滚，且拟撤单跨合约生效也不得
// 留下任何痕迹。
func TestRiskGapFillLaterCashPlusHoldingOverflowRollsBack(t *testing.T) {
	// 现金接近 int64 上界：A 合约 1 股 @100 成交后 cash=MaxInt64-200、pos A=1，
	// 开日基准 MaxInt64-100、亏损上限 10。
	cash0 := int64(math.MaxInt64 - 100)
	e, _ := NewEngine(cash0)
	mustSetMax(t, e, "A", 10)
	mustSetMax(t, e, "B", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 100}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	b0 := mustBuy(t, e, "A", 1, 100)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b0, Symbol: "A", Side: Buy, Price: 100, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 10); err != nil {
		t.Fatal(err)
	}

	// 开日后：A 待成交买单 1 股 @1；B 买单 2 股 @1 并部分成交 1 股
	// （cash=MaxInt64-201，B 持仓 1，两笔买单剩余占用现金各 1）；A 卖单 1 股。
	bA := mustBuy(t, e, "A", 1, 1)
	bB := mustBuy(t, e, "B", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bB, Symbol: "B", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	sellID := mustSell(t, e, "A", 1, 50)

	if e.Cash() != math.MaxInt64-201 || e.ReservedCash() != 2 ||
		e.Position("A") != 1 || e.Position("B") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d posA=%d posB=%d sellableA=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Position("B"), e.Sellable("A"))
	}

	// A 链：seq2@100 不触线；seq3@90 使净值 MaxInt64-110、亏损 10 达到上限
	// （本拟跨合约按编号降序先撤 bB 再撤 bA）；seq4@MaxInt64 下 A 持仓市值
	// MaxInt64 本身可表示，但现金 + 市值溢出 int64。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 90}, 0)
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: math.MaxInt64}, 0)

	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 100})
	if !errors.Is(err, ErrInt64Overflow) || n != 0 {
		t.Fatalf("现金加市值溢出必须整批失败且 0 条生效: n=%d err=%v", n, err)
	}

	// 基准、上限、未触线状态与最新报价全部保留；净值仍为提交前的可表示值。
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 100 {
		t.Fatalf("回滚后 A 最新报价应停留在 seq1@100: %+v", q)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != math.MaxInt64-100 || st.LossLimit != 10 {
		t.Fatalf("开日基准与上限必须保留: %+v", st)
	}
	if st.Restricted || st.Equity != math.MaxInt64-100 || st.Loss != 0 {
		t.Fatalf("拟触线不得留下，净值保持提交前值: %+v", st)
	}

	// 两笔买单（含跨合约的 B 部分成交买单）与 A 卖单全部保持提交前状态。
	if o, _ := e.Order(bA); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("A 待成交买单必须保持: %+v", o)
	}
	if o, _ := e.Order(bB); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 1 {
		t.Fatalf("B 部分成交买单必须保持: %+v", o)
	}
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("A 卖单必须保持有效: %+v", o)
	}
	if e.ReservedCash() != 2 || e.Cash() != math.MaxInt64-201 ||
		e.Position("A") != 1 || e.Position("B") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("现金、占用、持仓与卖单占用必须保持提交前值: reserved=%d cash=%d posA=%d posB=%d sellableA=%d",
			e.ReservedCash(), e.Cash(), e.Position("A"), e.Position("B"), e.Sellable("A"))
	}
	if !e.HasGap("A") {
		t.Fatal("回滚后 seq3/seq4 必须继续等待")
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
	if countRiskRecords(e) != 0 {
		t.Fatal("整批失败后不得存在触线记录")
	}
}
