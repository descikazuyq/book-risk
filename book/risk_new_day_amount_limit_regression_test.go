package book

import (
	"strings"
	"testing"
)

// 本文件为“开启新交易日后，账户总持仓金额上限仍继续约束买单”补充回归保障。
// 日内亏损保护在更大日号开日时重定基准、清除上一日增险限制，但账户总持仓金额
// 上限跨交易日保留、不随交易日重置；保护解除后的实际下单仍须遵守金额额度。
//
// 关键构造：初始现金 1000；A 合约限额 100、seq1@时刻10@价格10 已生效；
// 买入 10 股并全部按 10 成交（现金 900、持仓 10）。设置金额上限 120 后接受
// 2 股限价 10 的买单：持仓金额 100 + 买单占用 20，合计恰好 120。开启日号 1、
// 亏损上限 20（基准净值 1000），seq2@时刻20@价格8 生效使净值跌到 980、日内
// 亏损恰好 20，增险限制生效并撤销待成交买单（现金仍 900、持仓仍 10、占用归零）。
//
// 随后以日号 2、亏损上限 20 开新日：基准净值与当前净值均为 980、亏损归零、
// 限制解除；金额上限仍启用并保持 120，持仓金额 80，上一日撤销的订单保持撤销。
// 开日本身不新增触线或撤销记录，原有成交及保护记录保留各自发生时的数值。
//
// 此时买入 5 股限价 8 应成功：金额合计 80 + 5×max(8,8)=40 正好 120，占用现金
// 40，现金余额与已入账持仓不变。再申请 1 股限价 8 应因申请后合计 128 超过 120
// 被拒绝——拒绝原因必须是金额超限而非上一日亏损保护，也不得为接受申请撤销已有
// 买单；拒绝只新增一条记录、不生成订单，已接受的 5 股买单、两类金额与现金占用
// 保留。拒绝记录固化判断时持仓金额 80、买单占用 40、合计 120、申请后 128 以及
// A 的报价定位 seq2@时刻20@价格8；日内风险快照为日号 2、基准与净值 980、
// 亏损 0、上限 20、未限制。
func TestRiskNewDayAmountLimitStillConstrainsBuys(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	// 买入 10 股限价 10 并全部按 10 成交：现金 900、持仓 10。
	fillID := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: fillID, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 900 || e.Position("A") != 10 || e.ReservedCash() != 0 {
		t.Fatalf("成交后账实错误: cash=%d pos=%d reserved=%d",
			e.Cash(), e.Position("A"), e.ReservedCash())
	}

	// 金额上限 120；再接受 2 股限价 10 的买单：持仓金额 100 + 占用 20 = 120 恰好。
	if _, err := e.SetPositionAmountLimit(120); err != nil {
		t.Fatal(err)
	}
	day1Buy := mustBuy(t, e, "A", 2, 10)
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 120 ||
		st.Holding != 100 || st.BuyReserved != 20 || st.Total != 120 {
		t.Fatalf("合计应恰好达到上限 120: %+v", st)
	}

	// 开启日号 1、亏损上限 20：基准净值 = 现金 900 + 持仓 10×10 = 1000。
	if err := e.StartTradingDay(1, 20); err != nil {
		t.Fatal(err)
	}
	if st := e.RiskStatus(); !st.Open || st.Day != 1 || st.Baseline != 1000 ||
		st.Equity != 1000 || st.Loss != 0 || st.LossLimit != 20 || st.Restricted {
		t.Fatalf("日号 1 开日状态错误: %+v", st)
	}

	// seq2@时刻20@价格8：净值 980、日内亏损恰好 20 达到上限，增险限制生效，
	// 待成交买单被撤销；现金仍 900、持仓仍 10、买单占用归零。
	recsBeforeQuote := len(e.Records())
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 8}, 1)
	if st := e.RiskStatus(); !st.Open || st.Day != 1 || st.Baseline != 1000 ||
		st.Equity != 980 || st.Loss != 20 || st.LossLimit != 20 || !st.Restricted {
		t.Fatalf("seq2@8 应使亏损恰好 20 并限制增险: %+v", st)
	}
	if o, _ := e.Order(day1Buy); o.Status != StatusCanceled || o.Filled != 0 || o.Remaining() != 0 {
		t.Fatalf("待成交买单应被保护撤销: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.Position("A") != 10 {
		t.Fatalf("保护撤单只释放占用: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	// 本批只新增“当日首次触线 + 撤销买单”共 2 条记录，且锚定 seq2@(20,8)。
	quoteRecs := e.Records()[recsBeforeQuote:]
	if len(quoteRecs) != 2 || quoteRecs[0].Kind != RecordRiskTriggered ||
		quoteRecs[1].Kind != RecordCanceled {
		t.Fatalf("触线报价应只新增 1 触线 + 1 撤单记录: %+v", quoteRecs)
	}
	trig := quoteRecs[0]
	if trig.RiskTrigger != RiskTriggerQuote || trig.RiskRefSeq != 2 || trig.RiskRefMoment != 20 ||
		trig.RiskDay != 1 || trig.RiskBaseline != 1000 || trig.RiskEquity != 980 ||
		trig.RiskLoss != 20 || trig.RiskLimit != 20 || !trig.RiskRestrict {
		t.Fatalf("触线记录必须固化日号 1 在 seq2@8 下的快照: %+v", trig)
	}
	day1Cancel := quoteRecs[1]
	if day1Cancel.OrderID != day1Buy || day1Cancel.Remaining != 2 ||
		!strings.Contains(day1Cancel.Reason, "亏损 20") {
		t.Fatalf("撤单记录必须固化亏损保护原因: %+v", day1Cancel)
	}

	// 以日号 2、亏损上限 20 开启新日：重定基准、清除上一日限制；开日本身
	// 不新增任何记录，金额上限仍启用并保持 120，上一日撤销的订单保持撤销。
	recsBeforeDay2 := len(e.Records())
	if err := e.StartTradingDay(2, 20); err != nil {
		t.Fatalf("更大日号开日必须成功: %v", err)
	}
	if len(e.Records()) != recsBeforeDay2 {
		t.Fatalf("开日不得新增触线或撤销记录，实际新增 %d 条", len(e.Records())-recsBeforeDay2)
	}
	if st := e.RiskStatus(); !st.Open || st.Day != 2 || st.Baseline != 980 ||
		st.Equity != 980 || st.Loss != 0 || st.LossLimit != 20 || st.Restricted {
		t.Fatalf("日号 2 应重定基准 980、亏损归零、限制解除: %+v", st)
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 120 ||
		st.Holding != 80 || st.BuyReserved != 0 || st.Total != 80 {
		t.Fatalf("金额上限必须跨日保留，持仓金额按 seq2@8 为 80: %+v", st)
	}
	if o, _ := e.Order(day1Buy); o.Status != StatusCanceled {
		t.Fatalf("上一日撤销的订单不得恢复: %+v", o)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("开新日不得新增触线记录，上一日触线记录保留")
	}

	// 原有成交及保护记录继续保留各自发生时的数值：成交记录锚定 seq1@(10,10)、
	// 成交价 10；日号 1 的触线与撤单快照不被开日改写。
	var fillRec Record
	for _, r := range e.Records() {
		if r.Kind == RecordFilled && r.TradeID == 1 {
			fillRec = r
		}
	}
	if fillRec.TradeID != 1 || fillRec.TradePrice != 10 || fillRec.Qty != 10 ||
		!fillRec.QuoteValid || fillRec.QuoteSeq != 1 || fillRec.QuoteMoment != 10 || fillRec.QuotePrice != 10 {
		t.Fatalf("原成交记录必须保留成交时的报价与成交价: %+v", fillRec)
	}
	if again := e.Records()[recsBeforeQuote]; again.RiskDay != 1 || again.RiskBaseline != 1000 ||
		again.RiskEquity != 980 || again.RiskLoss != 20 || again.RiskLimit != 20 ||
		again.RiskRefSeq != 2 || again.RiskRefMoment != 20 {
		t.Fatalf("日号 1 的触线记录不得被开日改写: %+v", again)
	}
	if again := e.Records()[recsBeforeQuote+1]; again.OrderID != day1Buy ||
		again.Remaining != 2 || !strings.Contains(again.Reason, "亏损 20") {
		t.Fatalf("日号 1 的撤单记录不得被开日改写: %+v", again)
	}

	// 保护解除后买入 5 股限价 8：金额合计 80 + 5×max(8,8)=40 正好 120，接受；
	// 占用现金 40，现金余额与已入账持仓不变。
	day2Buy := mustBuy(t, e, "A", 5, 8)
	if day2Buy != day1Buy+1 {
		t.Fatalf("新买单应接续订单编号: got=%d want=%d", day2Buy, day1Buy+1)
	}
	if e.Cash() != 900 || e.ReservedCash() != 40 || e.AvailableCash() != 860 ||
		e.Position("A") != 10 {
		t.Fatalf("接受 5 股买单后账实错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 80 || st.BuyReserved != 40 ||
		st.Total != 120 || st.Limit != 120 {
		t.Fatalf("金额合计应恰好回到上限 120: %+v", st)
	}

	// 再申请 1 股限价 8：申请后合计 128 超过 120，必须拒绝；拒绝原因是金额
	// 超限而非上一日亏损保护，且不得为接受申请撤销已有买单。
	recsBeforeReject := len(e.Records())
	if _, err := e.Buy("A", 1, 8); err == nil {
		t.Fatal("申请后合计 128 超过上限 120 必须拒绝")
	} else {
		if !strings.Contains(err.Error(), "超过账户总持仓金额上限 120") {
			t.Fatalf("拒绝原因必须是金额超限: %v", err)
		}
		if strings.Contains(err.Error(), "亏损") {
			t.Fatalf("不得误报为上一日亏损保护: %v", err)
		}
	}
	newRecs := e.Records()[recsBeforeReject:]
	if len(newRecs) != 1 || newRecs[0].Kind != RecordRejected {
		t.Fatalf("拒绝只能新增一条拒绝记录: %+v", newRecs)
	}
	rj := newRecs[0]
	if !strings.Contains(rj.Reason, "超过账户总持仓金额上限 120") ||
		strings.Contains(rj.Reason, "亏损") {
		t.Fatalf("拒绝记录必须说明金额超限而非亏损保护: %q", rj.Reason)
	}
	// 拒绝记录固化判断时两类金额、合计、申请后合计与 A 的报价定位。
	if !rj.AmtEnabled || rj.AmtLimit != 120 || rj.AmtHolding != 80 ||
		rj.AmtBuyReserved != 40 || rj.AmtTotal != 120 || rj.AmtApplyTotal != 128 {
		t.Fatalf("拒绝记录金额快照错误: %+v", rj)
	}
	if len(rj.AmtQuoteRefs) != 1 ||
		rj.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 20, Price: 8}) {
		t.Fatalf("拒绝记录应保存 A 的报价定位 seq2@(20,8): %+v", rj.AmtQuoteRefs)
	}
	if !rj.QuoteValid || rj.QuoteSeq != 2 || rj.QuoteMoment != 20 || rj.QuotePrice != 8 {
		t.Fatalf("拒绝记录报价快照应锚定 seq2@(20,8): %+v", rj)
	}
	// 日内风险快照反映日号 2：基准与净值 980、亏损 0、上限 20、未限制。
	if rj.RiskDay != 2 || rj.RiskBaseline != 980 || rj.RiskEquity != 980 ||
		rj.RiskLoss != 0 || rj.RiskLimit != 20 || rj.RiskRestrict {
		t.Fatalf("拒绝记录应固化日号 2 未限制的风险快照: %+v", rj)
	}

	// 拒绝不生成订单、不撤销已有买单：已接受的 5 股买单、两类金额与现金占用保留。
	if _, ok := e.Order(day2Buy + 1); ok {
		t.Fatal("被拒绝的申请不得生成订单")
	}
	if o, _ := e.Order(day2Buy); o.Status != StatusPending || o.Qty != 5 ||
		o.Limit != 8 || o.Remaining() != 5 {
		t.Fatalf("已接受的 5 股买单必须保留: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 40 || e.Position("A") != 10 {
		t.Fatalf("拒绝不得改动现金与占用: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 80 || st.BuyReserved != 40 ||
		st.Total != 120 || st.Limit != 120 {
		t.Fatalf("拒绝不得改动两类金额: %+v", st)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("金额拒绝不得产生新的触线记录")
	}
}
