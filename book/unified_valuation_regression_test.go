package book

import (
	"testing"
)

// 统一持仓估值核心的回归测试：日内亏损保护与账户总持仓金额上限共用同一套
// “数量 × 最新已生效报价”的持仓估值规则，但两者的报价定位范围不同。
//
// 场景对齐口径说明：初始现金 1000，合约 A 买入成交 10 份 @10 后现金余额 900；
// A 最新报价 12，其中 3 份被一笔有效卖单占用——成交前这 3 份仍属于持仓，
// 持仓金额仍为 120；账户另有一笔 A 的有效买单（A 同时有持仓和买单），
// 合约 B 只有一笔有效买单、没有持仓。

func setupUnifiedValuation(t *testing.T) *Engine {
	t.Helper()
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 1000)
	mustSetMax(t, e, "B", 1000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)

	idA := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: idA, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatalf("A 买入成交 10 份: %v", err)
	}
	if e.Cash() != 900 || e.Position("A") != 10 {
		t.Fatalf("成交后应为现金 900、持仓 10: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}

	// A 报价升到 12：持仓市值 120。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 12}, 1)

	// 卖单占用 3 份：成交前不抵减持仓，也不产生金额。
	mustSell(t, e, "A", 3, 1)
	if e.Sellable("A") != 7 {
		t.Fatalf("卖单占用 3 份后可卖应为 7: %d", e.Sellable("A"))
	}

	// A 再挂买单 2 份（限价 10 < 报价 12，金额占用按报价 12 计 24），使 A 同时
	// 有持仓和买单；B 报价 20 后挂买单 1 份（占用 20），B 始终无持仓。
	mustBuy(t, e, "A", 2, 10)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 20}, 1)
	mustBuy(t, e, "B", 1, 20)
	return e
}

func TestUnifiedValuationHoldingIgnoresSellReservationAndBuyCashFreeze(t *testing.T) {
	e := setupUnifiedValuation(t)

	// 金额上限恰好设为当前合计 164：持仓 120（卖单占用的 3 份不抵减）
	// + A 买单 2×max(10,12)=24 + B 买单 1×20=20。
	if _, err := e.SetPositionAmountLimit(164); err != nil {
		t.Fatal(err)
	}
	st := e.PositionAmountStatus()
	if st.Holding != 120 || st.BuyReserved != 44 || st.Total != 164 {
		t.Fatalf("持仓金额必须仍为 120（3 份卖单占用不抵减），买单占用 44: %+v", st)
	}

	// 开日基准与当前净值：现金余额 900 + 持仓市值 120 = 1020。
	// 买单冻结的 40 现金本就留在余额中，不能再从净值扣除；买单的 max 占用
	// 属于金额上限，不能带进净值。
	if err := e.StartTradingDay(1, 100000); err != nil {
		t.Fatal(err)
	}
	rs := e.RiskStatus()
	if rs.Baseline != 1020 || rs.Equity != 1020 || rs.Loss != 0 {
		t.Fatalf("净值必须为 现金900 + 持仓120 = 1020，买单冻结现金不重复扣: %+v", rs)
	}
}

func TestUnifiedValuationQuoteRefsDifferByProtection(t *testing.T) {
	e := setupUnifiedValuation(t)
	if _, err := e.SetPositionAmountLimit(164); err != nil {
		t.Fatal(err)
	}

	// 再买 A 1 份（限价 1）：申请金额 1×max(1,12)=12，申请后合计 176 > 164，
	// 由账户金额上限拒绝。
	recsBefore := len(e.Records())
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("申请后合计超过金额上限必须拒绝")
	}
	rj := e.Records()[recsBefore]
	if rj.Kind != RecordRejected {
		t.Fatalf("应为拒绝记录: %+v", rj)
	}
	if rj.AmtHolding != 120 || rj.AmtBuyReserved != 44 || rj.AmtTotal != 164 || rj.AmtApplyTotal != 176 {
		t.Fatalf("金额拒绝快照错误: %+v", rj)
	}
	// 金额判断定位：A（既有持仓又有买单）只能出现一次；只有买单的 B 必须出现；
	// 定位按合约代码排列，并保存真正参与计算的序号、时刻与价格。
	if len(rj.AmtQuoteRefs) != 2 {
		t.Fatalf("金额定位应只有 A、B 各一条（A 持仓+买单不重复）: %+v", rj.AmtQuoteRefs)
	}
	if rj.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 12}) {
		t.Fatalf("金额定位 A 必须锚定 seq2/时刻2/价格12: %+v", rj.AmtQuoteRefs[0])
	}
	if rj.AmtQuoteRefs[1] != (RiskQuoteRef{Symbol: "B", Seq: 1, Moment: 1, Price: 20}) {
		t.Fatalf("金额定位必须包含只有买单的 B（seq1/时刻1/价格20）: %+v", rj.AmtQuoteRefs[1])
	}

	if err := e.StartTradingDay(1, 100000); err != nil {
		t.Fatal(err)
	}

	// 下调亏损上限到 0 立即触线：净值持仓报价定位只解释真正贡献持仓市值的合约，
	// 因此只含持仓合约 A；只有买单的 B 不得出现。
	touchBefore := len(e.Records())
	if err := e.SetLossLimit(0); err != nil {
		t.Fatal(err)
	}
	var trig Record
	for _, r := range e.Records()[touchBefore:] {
		if r.Kind == RecordRiskTriggered {
			trig = r
			break
		}
	}
	if trig.Kind != RecordRiskTriggered {
		t.Fatalf("零亏损上限应立即产生触线记录")
	}
	if trig.RiskEquity != 1020 || trig.RiskLoss != 0 || trig.RiskLimit != 0 {
		t.Fatalf("触线记录应固化净值 1020: %+v", trig)
	}
	if len(trig.RiskQuoteRefs) != 1 {
		t.Fatalf("净值持仓报价定位必须只含持仓合约 A，不含只有买单的 B: %+v", trig.RiskQuoteRefs)
	}
	if trig.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 12}) {
		t.Fatalf("触线定位必须锚定 A seq2/时刻2/价格12: %+v", trig.RiskQuoteRefs[0])
	}

	// 触线撤单顺序仍为订单编号从大到小：B 买单编号更大，先于 A 买单被撤。
	var cancels []Record
	for _, r := range e.Records()[touchBefore:] {
		if r.Kind == RecordCanceled {
			cancels = append(cancels, r)
		}
	}
	if len(cancels) != 2 || cancels[0].Symbol != "B" || cancels[1].Symbol != "A" {
		t.Fatalf("触线应按编号从大到小先撤 B 再撤 A: %+v", cancels)
	}

	// 调用方篡改查询得到的记录副本，不得改写已保存的报价事实（统一核心的
	// 定位切片在保存时已与引擎内部状态脱离）。
	tampered := e.Records()
	var got Record
	for i := range tampered {
		if tampered[i].Kind == RecordRiskTriggered {
			got = tampered[i]
			break
		}
	}
	got.RiskQuoteRefs[0].Price = 999
	rj.AmtQuoteRefs[0].Price = 999
	rj.AmtQuoteRefs[1].Seq = 99
	again := e.Records()
	var fresh Record
	for i := range again {
		if again[i].Kind == RecordRiskTriggered {
			fresh = again[i]
			break
		}
	}
	if fresh.RiskQuoteRefs[0].Price != 12 {
		t.Fatalf("篡改查询副本不得改写触线报价定位: %+v", fresh.RiskQuoteRefs)
	}
	var freshRej Record
	for i := range again {
		if again[i].Kind == RecordRejected && again[i].AmtEnabled && again[i].AmtApplyTotal == 176 {
			freshRej = again[i]
			break
		}
	}
	if freshRej.AmtQuoteRefs[0].Price != 12 || freshRej.AmtQuoteRefs[1].Seq != 1 {
		t.Fatalf("篡改查询副本不得改写金额拒绝报价定位: %+v", freshRej.AmtQuoteRefs)
	}
}
