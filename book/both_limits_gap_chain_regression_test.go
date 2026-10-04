package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// 本文件为“补齐行情缺口”补充组合保护的回归保障：日内亏损保护与账户总持仓
// 金额上限同时启用时，由亏损保护在链中撤销的买单，其剩余量不再参与链中及
// 批末的账户金额计算；但已成交入账的真实持仓始终计入，撤单排除规则不能
// 扩大为“忽略持仓”。
//
// 关键构造（成功路径）：
//   - 初始现金 1_000_000，A 合约在 seq1@10 下购入并部分成交：
//     全成交买单 2 股 @10（cash=999_980，pos=2），再下 10 股限价 10 的买单
//     并部分成交 2 股 @10（cash=999_960，pos=4，剩余 8 股占用现金 80）；
//   - 开日（日号 1，亏损上限 30，基准 1_000_000），再设金额上限 120
//     （seq1@10 下持仓金额 40 + 买单剩余占用 80 = 120，恰满，设置时不撤单）；
//   - 后续序号先到并等待：seq3 取 MaxInt64/6（记 P）——真实持仓 4 股 ×P 仍
//     可用 int64 表示，但若错误地把触线撤销的 8 股剩余量继续计入金额，
//     8×P 必然溢出 int64；
//   - 调用方补齐缺失的 seq2@1，链为 seq2@1 → seq3@P：seq2@1 下净值
//     999_960 + 4×1 = 999_964，日内亏损 36 触线（上限 30），亏损保护撤销
//     bPart 全部未成交部分（取消 8 股，释放现金占用 80；未成交的 8 股从未
//     入账，不是持仓，真实持仓始终只有 4 股）；seq3@P 只面对“持仓 4、无
//     有效买单”，4×P 可表示、8×P 不计入，整批成功。
//
// 关键构造（回滚路径，第二个测试）：
//   - 同一组合启用状态，seq3 取 MaxInt64/4+1（=P4）——4×P4 溢出（真实持仓
//     市值溢出），即使 seq2 已撤光买单；预检测出后整批不生效，只留一条拒绝。
//
// 成功路径要求：seq2、seq3 共 2 条报价生效，最新报价推进到批末 seq3@P，
// 缺口消失；中间报价触线时撤销该买单全部未成交部分并释放相应现金占用；已成交数量、持仓

// 和现金余额保持原值；末条价格回升后限制仍保留，账户金额查询按末条报价
// 计算真实持仓，买单占用为零；触线与撤销记录对应真正触线的中间报价 seq2，
// 保留当时亏损、上限与报价定位，撤单原因仍是日内亏损保护（不变成金额上限
// 原因，也不额外产生金额溢出的拒绝记录）。
//
// 等待中的报价在补齐前不得提前改变这些结果。
//
// 回滚路径要求：后续报价使仍保留的持仓市值超出 int64 范围时，整批返回包装
// ErrInt64Overflow 的错误与零条生效结果；报价、订单、资金占用与日内限制
// 维持提交前状态，原先等待的报价继续等待；只新增一条说明溢出的拒绝记录，
// 不留下这批中间报价拟产生的触线或撤销记录。

// bothLimitsGapSetup 构造两项保护都启用的共用前置状态。
//
//	全成交买单 bFull：A 2 股 @10 -> cash=999_980, pos=2
//	部分成交买单 bPart：A 10 股限价 10，已成交 2 @10 -> cash=999_960,
//	  pos=4，剩余 8 股现金占用 80
//	开日：日号 1、上限 30、基准 1_000_000
//	金额上限 120：seq1@10 下持仓 40 + 买单占用 80 = 120（恰满）
func bothLimitsGapSetup(t *testing.T) (e *Engine, bFull, bPart int64) {
	t.Helper()
	e, _ = NewEngine(1_000_000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bFull = mustBuy(t, e, "A", 2, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bFull, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	bPart = mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bPart, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(120); err != nil {
		t.Fatal(err)
	}

	if e.Cash() != 999_960 || e.ReservedCash() != 80 || e.Position("A") != 4 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Holding != 40 ||
		st.BuyReserved != 80 || st.Total != 120 {
		t.Fatalf("前置金额状态错误: %+v", st)
	}
	return e, bFull, bPart
}

// TestBothLimitsGapFillRiskCancelExcludedFromLaterAmount 覆盖核心成功路径：
// seq2@1 使日内亏损 36 触线（上限 30），亏损保护撤销 bPart 全部未成交的 8 股；
// 批末 seq3@P 下，真实持仓 4×P 仍可用 int64 表示，但若把已撤的 8 股剩余量
// 继续计入金额，8×P 必然溢出。补齐必须成功：2 条生效、最新报价到 seq3、缺口
// 消失，不能因已取消的数量报金额溢出；也不额外产生金额超限撤单或拒绝。
func TestBothLimitsGapFillRiskCancelExcludedFromLaterAmount(t *testing.T) {
	e, bFull, bPart := bothLimitsGapSetup(t)

	// 后续序号的回升（天价）报价先到并等待：8×P 溢出、4×P 可表示。
	pRecover := int64(math.MaxInt64 / 6)
	wantHolding := pRecover * 4
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: pRecover}, 0)
	if !e.HasGap("A") {
		t.Fatal("seq3 跨号必须进入等待")
	}

	// 等待中的报价不得提前改变估值、金额、风险状态或订单。
	if rs := e.RiskStatus(); rs.Restricted || rs.Equity != 1_000_000 || rs.Loss != 0 {
		t.Fatalf("等待报价不得提前参与估值: %+v", rs)
	}
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 80 || st.Total != 120 {
		t.Fatalf("等待报价不得提前改变金额状态: %+v", st)
	}
	if o, _ := e.Order(bPart); o.Status != StatusPartial || o.Remaining() != 8 {
		t.Fatalf("等待期间部分成交买单必须保持: %+v", o)
	}

	// 调用方补齐缺失的 seq2@1：seq2 触线撤 8 股 -> seq3 只面对持仓 4，成功。
	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 1})
	if err != nil {
		t.Fatalf("已撤买单不得被批末报价计入致整批溢出失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("本次应真正生效 2 条报价（seq2、seq3），实际 %d", n)
	}

	// 中间报价触线：撤销 bPart 全部未成交部分（取消 8 股），已成交 2 股与
	// 全成交 bFull 的 2 股持仓保留；bFull 已全部成交，本就无剩余量可撤。
	oP, _ := e.Order(bPart)
	if oP.Status != StatusCanceled || oP.Filled != 2 || oP.Remaining() != 0 {
		t.Fatalf("部分成交买单只撤销未成交的 8 股、保留已成交 2 股: %+v", oP)
	}
	if !strings.Contains(oP.Reason, "亏损 36") || !strings.Contains(oP.Reason, "上限 30") {
		t.Fatalf("撤单原因必须是日内亏损保护并固化触线时亏损 36、上限 30: %q", oP.Reason)
	}
	if strings.Contains(oP.Reason, "账户总持仓金额") {
		t.Fatalf("亏损保护撤单原因不能被金额上限改写: %q", oP.Reason)
	}
	oF, _ := e.Order(bFull)
	if oF.Status != StatusFilled || oF.Filled != 2 {
		t.Fatalf("全成交买单保持全部成交: %+v", oF)
	}

	// 已成交数量、持仓与现金余额保持原值；被撤 8 股的现金占用 80 必须释放。
	if e.Position("A") != 4 {
		t.Fatalf("已成交持仓必须保留 4 股: %d", e.Position("A"))
	}
	if e.Cash() != 999_960 {
		t.Fatalf("无新增成交时现金余额不得变化: %d", e.Cash())
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("被撤订单剩余现金占用必须释放: %d", e.ReservedCash())
	}

	// 末条价格回升后日内限制仍保留；账户查询按末条 seq3@P 计算真实持仓，
	// 买单占用为零——既不能为避开溢出而漏算持仓，也不能沿用触线时低价。
	rs := e.RiskStatus()
	if !rs.Restricted {
		t.Fatal("中间报价触线后，即使批末价格大幅回升，限制也必须保留")
	}
	wantEquity := int64(999_960) + wantHolding
	if rs.Equity != wantEquity || rs.Loss != 0 {
		t.Fatalf("风险净值必须按批末 seq3@P 对真实持仓估值: equity=%d want=%d loss=%d",
			rs.Equity, wantEquity, rs.Loss)
	}
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Limit != 120 {
		t.Fatalf("金额上限设置必须保留: %+v", st)
	}
	if st.Holding != wantHolding || st.BuyReserved != 0 || st.Total != wantHolding {
		t.Fatalf("批末金额: 真实持仓 4×P 计入、已撤买单占用为零: %+v wantHolding=%d",
			st, wantHolding)
	}

	// 最新报价推进到末条，缺口消失。
	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 3 || q.Price != pRecover {
		t.Fatalf("最新报价应推进到批末 seq3@P: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("链全部生效后缺口必须消失")
	}

	// 本批只新增“触线 1 条 + 亏损撤单 1 条”共 2 条记录；不得产生金额上限
	// 撤销记录，也不得因已取消数量产生任何金额溢出的拒绝记录。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 2 {
		t.Fatalf("应只新增 2 条记录（1 触线 + 1 撤单），实际 %d 条: %+v", len(newRecs), newRecs)
	}
	trig := newRecs[0]
	if trig.Kind != RecordRiskTriggered {
		t.Fatalf("第一条新增记录必须是触线记录: %+v", trig)
	}
	if trig.RiskTrigger != RiskTriggerQuote || trig.RiskRefSeq != 2 || trig.RiskRefMoment != 2 {
		t.Fatalf("触线来源与定位必须固化在真正触线的中间报价 seq2@时刻2: %s/%d/%d",
			trig.RiskTrigger, trig.RiskRefSeq, trig.RiskRefMoment)
	}
	if trig.RiskDay != 1 || trig.RiskBaseline != 1_000_000 || trig.RiskEquity != 999_964 ||
		trig.RiskLoss != 36 || trig.RiskLimit != 30 || !trig.RiskRestrict {
		t.Fatalf("触线快照必须固化 seq2@1 下的亏损 36 与上限 30: %+v", trig)
	}
	if len(trig.RiskQuoteRefs) != 1 ||
		trig.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 1}) {
		t.Fatalf("参与估值的报价定位必须锚定 seq2@1: %+v", trig.RiskQuoteRefs)
	}

	cr := newRecs[1]
	if cr.Kind != RecordCanceled || cr.OrderID != bPart || cr.Filled != 2 || cr.Remaining != 8 {
		t.Fatalf("第二条记录必须是 bPart 的撤销，取消剩余 8 股、已成交 2 股: %+v", cr)
	}
	if cr.QuoteSeq != 2 || cr.QuotePrice != 1 || cr.QuoteMoment != 2 {
		t.Fatalf("撤单报价快照应锚定触线时的 seq2@1: %+v", cr)
	}
	if cr.RiskEquity != 999_964 || cr.RiskLoss != 36 || cr.RiskLimit != 30 || !cr.RiskRestrict {
		t.Fatalf("撤单记录应携带触线时风险快照: %+v", cr)
	}
	// 亏损保护撤单不应携带金额上限明细，也不能是金额上限撤销。
	if cr.AmtEnabled {
		t.Fatalf("日内亏损保护撤单不应固化金额上限明细: %+v", cr)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 限制保留：回升后新买单继续被拒，且不产生金额相关副作用。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("限制保留期间新买单必须拒绝")
	}
}

// TestBothLimitsGapFillLaterHoldingOverflowRollsBack 覆盖边界：两项保护同时
// 启用、seq2 本会触线并撤光买单剩余量，但批末 seq3 天价使仍保留的真实持仓
// 4×P4 溢出 int64。撤单排除规则不能扩大为忽略持仓——整批必须回滚，返回包装
// ErrInt64Overflow 的错误与 0 条生效；报价、订单、资金占用与日内限制维持
// 提交前状态，等待报价继续等待；只新增一条说明溢出的拒绝记录，不留下这批
// 中间报价拟产生的触线或撤销记录。
func TestBothLimitsGapFillLaterHoldingOverflowRollsBack(t *testing.T) {
	e, bFull, bPart := bothLimitsGapSetup(t)

	// 4×P4 溢出（真实持仓市值溢出）；即使 seq2 已撤光买单，持仓仍不可表示。
	pOverflow := int64(math.MaxInt64/4 + 1)
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: pOverflow}, 0)

	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 1})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("批末真实持仓市值溢出必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if n != 0 {
		t.Fatalf("整批失败时生效条数必须为零，实际 %d", n)
	}

	// 最新报价、开日基准、亏损上限与未触线状态全部保留。
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 10 {
		t.Fatalf("回滚后最新报价应停留在 seq1@10: %+v", q)
	}
	rs := e.RiskStatus()
	if !rs.Open || rs.Day != 1 || rs.Baseline != 1_000_000 || rs.LossLimit != 30 {
		t.Fatalf("开日基准与上限必须保留: %+v", rs)
	}
	if rs.Restricted || rs.Equity != 1_000_000 || rs.Loss != 0 {
		t.Fatalf("拟触发的触线不得留下，净值保持提交前估值: %+v", rs)
	}

	// 链中本拟触发的亏损撤单不得改变订单：bPart 保持部分成交、剩余 8。
	if o, _ := e.Order(bPart); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 8 {
		t.Fatalf("部分成交买单必须保持部分成交、剩余 8: %+v", o)
	}
	if o, _ := e.Order(bFull); o.Status != StatusFilled || o.Filled != 2 {
		t.Fatalf("全成交买单保持全部成交: %+v", o)
	}

	// 现金、买单占用与持仓全部维持提交前值。
	if e.Cash() != 999_960 || e.ReservedCash() != 80 || e.Position("A") != 4 {
		t.Fatalf("资金与持仓必须保持提交前值: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	// 金额状态恢复提交前的 seq1@10 口径（持仓 40 + 买单 80 = 120）。
	if st := e.PositionAmountStatus(); !st.Enabled || st.Holding != 40 ||
		st.BuyReserved != 80 || st.Total != 120 {
		t.Fatalf("金额状态应恢复提交前值: %+v", st)
	}

	// 原先等待的 seq3 继续等待，缺失的 seq2 未被吞掉。
	if !e.HasGap("A") {
		t.Fatal("回滚后等待中的 seq3 必须继续等待")
	}

	// 只新增一条说明溢出的拒绝记录；不得留下拟产生的触线或撤销记录。
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("整批失败只能新增一条记录，实际 %d 条", len(recs)-recsBefore)
	}
	rj := recs[recsBefore]
	if rj.Kind != RecordRejected {
		t.Fatalf("唯一新增记录必须是拒绝记录: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "序号 3") || !strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因必须指明链中溢出的 seq3 与 int64: %q", rj.Reason)
	}
	// 拒绝记录固化提交前（seq1@10）的报价与风险快照。
	if rj.QuoteSeq != 1 || rj.QuotePrice != 10 {
		t.Fatalf("拒绝记录的报价快照应停留在 seq1@10: %+v", rj)
	}
	if rj.RiskDay != 1 || rj.RiskBaseline != 1_000_000 || rj.RiskEquity != 1_000_000 ||
		rj.RiskLoss != 0 || rj.RiskLimit != 30 || rj.RiskRestrict {
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

	// 等待报价同内容重复提交仍是无操作（继续等待）；再次补齐仍因同一溢出失败。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: pOverflow}, 0)
	if n2, err2 := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 1}); !errors.Is(err2, ErrInt64Overflow) || n2 != 0 {
		t.Fatalf("等待报价保留时，重新补齐必须仍可识别同一溢出: n=%d err=%v", n2, err2)
	}
}
