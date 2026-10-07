package book

import (
	"errors"
	"strings"
	"testing"
)

// 本文件为“开启新交易日后，账户总持仓金额上限仍继续约束买单”补充自动回归保障。
//
// 既有功能在更大日号开始时会重定日内亏损基准、清除上一日的增险限制，但账户总持仓
// 金额上限跨交易日保留；上一日被亏损保护撤销的买单保持撤销状态，保护解除后的实际
// 下单仍必须遵守金额额度。全部路径只用公开的交易与查询功能驱动与核对。
//
// 关键构造：初始现金 1000；合约 A 持仓限额 100，序号 1、时刻 10、价格 10 的报价
// 已生效。限价 10 买入 10 份并全部按价格 10 成交后，现金 900、持仓 10。随后设置
// 账户总持仓金额上限 120（持仓金额 100、买单占用 0，不撤单），再接受 2 份限价 10
// 的买单：持仓金额 100、买单占用 20，合计恰好 120。
//
// 以日号 1、亏损上限 20 开日（基准净值 1000），再让序号 2、时刻 20、价格 8 的报价
// 生效：现金 900 + 持仓 10×8 = 980，日内亏损恰好 20，增险限制生效，待成交的 2 份
// 买单被撤销、买单占用归零，现金仍 900、持仓仍 10。
//
// 随后以日号 2、亏损上限 20 开启新日：基准净值与当前净值均为 980，日内亏损归零、
// 限制解除；金额上限仍启用并保持 120，持仓金额仍为 80，上一日撤销的订单保持撤销。
// 开日本身不新增触线或撤销记录，原有成交及保护记录继续保留各自发生时的数值。
//
// 此时买入 5 份限价 8 必须成功：持仓金额 80 + 买单占用 40 正好 120，占用现金 40，
// 现金余额与已入账持仓不变。再申请 1 份限价 8 必须因申请后金额 128 超过 120 而被
// 拒绝——不能误报为上一日亏损保护，也不能为接受申请撤销任何已有买单；拒绝只新增
// 一条说明金额超限的记录、不生成订单，已接受的 5 份买单与两类金额、现金占用保留。
// 拒绝记录保存判断时持仓金额 80、买单占用 40、合计 120、申请后 128，以及 A 的
// 序号 2、时刻 20、价格 8；日内风险快照反映日号 2、基准与净值 980、亏损 0、上限
// 20、未限制。

// setupCrossDayAmountLimit 构造“日号 1 亏损恰好触线、买单已被保护撤销”的共用前置
// 状态，并返回引擎、编号 1（10 份全部成交的买单）、编号 2（2 份被撤销的买单）以及
// 此刻的记录条数。此时：现金 900、A 持仓 10、买单占用 0；金额上限 120 已启用，
// 持仓金额 80；日号 1 已限制，最新报价 seq2(时刻20)@8。
func setupCrossDayAmountLimit(t *testing.T) (e *Engine, filledID, canceledID int64, recsAfterDay1 int) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	filledID = mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: filledID, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.Position("A") != 10 {
		t.Fatalf("成交后前置状态错误: cash=%d reserved=%d pos=%d", e.Cash(), e.ReservedCash(), e.Position("A"))
	}

	// 设置金额上限 120：当前仅持仓金额 100，不触发任何撤单。
	if ids, err := e.SetPositionAmountLimit(120); err != nil {
		t.Fatalf("设置金额上限失败: %v", err)
	} else if len(ids) != 0 {
		t.Fatalf("合计 100 未超过上限 120，不应撤销订单，实际撤销 %v", ids)
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 120 ||
		st.Holding != 100 || st.BuyReserved != 0 || st.Total != 100 {
		t.Fatalf("设置上限后金额状态错误: %+v", st)
	}

	// 2 份限价 10 的买单：持仓金额 100 + 买单占用 20 恰好 120。
	canceledID = mustBuy(t, e, "A", 2, 10)
	if e.Cash() != 900 || e.ReservedCash() != 20 || e.AvailableCash() != 880 || e.Position("A") != 10 {
		t.Fatalf("接受 2 份买单后资金状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 100 || st.BuyReserved != 20 || st.Total != 120 {
		t.Fatalf("接受 2 份买单后金额合计应恰好为 120: %+v", st)
	}

	// 日号 1、亏损上限 20：基准 = 900 + 10×10 = 1000，零亏损不触线、不产生记录。
	recsBeforeDay1 := len(e.Records())
	if err := e.StartTradingDay(1, 20); err != nil {
		t.Fatalf("日号 1 合法开日必须成功: %v", err)
	}
	if len(e.Records()) != recsBeforeDay1 {
		t.Fatalf("零亏损开日不得新增记录，实际新增 %d 条", len(e.Records())-recsBeforeDay1)
	}
	if st := e.RiskStatus(); !st.Open || st.Day != 1 || st.Baseline != 1000 || st.Equity != 1000 ||
		st.Loss != 0 || st.LossLimit != 20 || st.Restricted {
		t.Fatalf("日号 1 开日状态错误: %+v", st)
	}

	// seq2(时刻20)@8 生效：净值 980、亏损恰好 20，触线并撤销 2 份待成交买单。
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 8})
	if err != nil || n != 1 {
		t.Fatalf("seq2@8 应生效 1 条: n=%d err=%v", n, err)
	}
	st := e.RiskStatus()
	if !st.Restricted || st.Equity != 980 || st.Loss != 20 {
		t.Fatalf("seq2@8 后应净值 980、亏损 20 并已限制: %+v", st)
	}
	o, _ := e.Order(canceledID)
	if o.Status != StatusCanceled || o.Filled != 0 || o.Remaining() != 0 {
		t.Fatalf("2 份待成交买单必须被亏损保护整单撤销: %+v", o)
	}
	if !strings.Contains(o.Reason, "亏损 20") || !strings.Contains(o.Reason, "上限 20") {
		t.Fatalf("撤单原因必须固化亏损 20 与上限 20: %q", o.Reason)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.Position("A") != 10 {
		t.Fatalf("触线撤单后现金 900、持仓 10 不变，占用归零: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if amt := e.PositionAmountStatus(); !amt.Enabled || amt.Limit != 120 ||
		amt.Holding != 80 || amt.BuyReserved != 0 || amt.Total != 80 {
		t.Fatalf("触线撤单后金额上限仍启用，合计应只剩持仓金额 80: %+v", amt)
	}
	if countRiskRecords(e) != 1 {
		t.Fatalf("日号 1 应恰好有一条触线记录，实际 %d 条", countRiskRecords(e))
	}
	return e, filledID, canceledID, len(e.Records())
}

// TestStartNewTradingDayKeepsAmountLimitConstrainingBuys 覆盖主路径：日号 2 重定
// 基准、解除上一日增险限制，但金额上限 120 跨日保留；恰好 120 的 5 份买单接受，
// 申请后 128 的 1 份买单按金额上限拒绝（而非上一日亏损保护），拒绝不撤任何已有
// 买单、不生成订单，只追加一条固化判断时金额与报价、日号 2 风险快照的拒绝记录。
func TestStartNewTradingDayKeepsAmountLimitConstrainingBuys(t *testing.T) {
	e, filledID, canceledID, recsAfterDay1 := setupCrossDayAmountLimit(t)

	// 以更大日号 2、同样亏损上限 20 开启新日：基准与净值均为 980，亏损归零、限制
	// 解除；开日不新增任何记录。
	if err := e.StartTradingDay(2, 20); err != nil {
		t.Fatalf("日号 2 合法开日必须成功: %v", err)
	}
	if len(e.Records()) != recsAfterDay1 {
		t.Fatalf("开日不得新增触线或撤销记录，实际新增 %d 条", len(e.Records())-recsAfterDay1)
	}
	rs := e.RiskStatus()
	if !rs.Open || rs.Day != 2 || rs.Baseline != 980 || rs.Equity != 980 ||
		rs.Loss != 0 || rs.LossLimit != 20 || rs.Restricted {
		t.Fatalf("日号 2 应基准与净值均为 980、亏损 0、限制解除: %+v", rs)
	}
	// 金额上限跨交易日保留，持仓金额仍为 80。
	amt := e.PositionAmountStatus()
	if !amt.Enabled || amt.Limit != 120 || amt.Holding != 80 || amt.BuyReserved != 0 || amt.Total != 80 {
		t.Fatalf("跨日后金额上限 120 必须保留且持仓金额仍为 80: %+v", amt)
	}
	// 上一日撤销的订单保持撤销状态，已全部成交的订单不变。
	if o, _ := e.Order(canceledID); o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("上一日撤销的买单跨日后必须保持撤销: %+v", o)
	}
	if o, _ := e.Order(filledID); o.Status != StatusFilled || o.Filled != 10 {
		t.Fatalf("已成交订单跨日后必须保持全部成交: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.Position("A") != 10 {
		t.Fatalf("跨日不得改动资金与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("跨日不得新增触线记录，日号 1 的触线记录保留")
	}

	// 买入 5 份限价 8：持仓金额 80 + 买单占用 5×8=40，合计正好 120，必须接受。
	newID := mustBuy(t, e, "A", 5, 8)
	if e.Cash() != 900 || e.ReservedCash() != 40 || e.AvailableCash() != 860 || e.Position("A") != 10 {
		t.Fatalf("接受 5 份买单只冻结 40 现金，余额 900 与持仓 10 不变: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 80 || st.BuyReserved != 40 || st.Total != 120 {
		t.Fatalf("接受后持仓金额 80、买单占用 40、合计恰好 120: %+v", st)
	}
	if o, _ := e.Order(newID); o.Status != StatusPending || o.Qty != 5 || o.Limit != 8 || o.Remaining() != 5 {
		t.Fatalf("5 份买单必须保持待成交: %+v", o)
	}
	// 接受记录携带日号 2、未限制的风险快照与最新报价 seq2@(20,8)。
	acceptRec := e.Records()[len(e.Records())-1]
	if acceptRec.Kind != RecordAccepted || acceptRec.OrderID != newID {
		t.Fatalf("5 份买单的最新记录必须是其接受记录: %+v", acceptRec)
	}
	if acceptRec.QuoteSeq != 2 || acceptRec.QuoteMoment != 20 || acceptRec.QuotePrice != 8 {
		t.Fatalf("接受记录报价快照应为 seq2(20,8): %+v", acceptRec)
	}
	if acceptRec.RiskDay != 2 || acceptRec.RiskBaseline != 980 || acceptRec.RiskEquity != 980 ||
		acceptRec.RiskLoss != 0 || acceptRec.RiskLimit != 20 || acceptRec.RiskRestrict {
		t.Fatalf("接受记录应携带日号 2、亏损 0、未限制的风险快照: %+v", acceptRec)
	}

	// 再申请 1 份限价 8：申请后 80+40+8=128 超过 120，必须按金额上限拒绝。
	ordersBefore := len(e.Orders())
	recsBefore := len(e.Records())
	_, err := e.Buy("A", 1, 8)
	if err == nil {
		t.Fatal("申请后合计 128 超过上限 120 必须拒绝")
	}
	if !strings.Contains(err.Error(), "超过账户总持仓金额上限") {
		t.Fatalf("拒绝原因必须说明金额超限: %v", err)
	}
	if strings.Contains(err.Error(), "日内亏损保护") {
		t.Fatalf("不得误报为上一日亏损保护: %v", err)
	}
	if errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("128 为正常超限，不应是 int64 溢出错误: %v", err)
	}

	// 只新增一条拒绝记录，不生成订单（不消耗订单编号），也不产生撤单记录。
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("只能新增一条拒绝记录，实际新增 %d 条", len(recs)-recsBefore)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rj)
	}
	for _, r := range recs[recsBefore:] {
		if r.Kind == RecordCanceled {
			t.Fatalf("金额超限拒绝不得为接受申请撤销任何已有买单: %+v", r)
		}
	}
	if len(e.Orders()) != ordersBefore {
		t.Fatalf("被金额上限拒绝的买单不得生成订单，订单数由 %d 变为 %d",
			ordersBefore, len(e.Orders()))
	}

	// 拒绝记录固化申请参数与判断时事实：持仓金额 80、买单占用 40、合计 120、
	// 申请后 128；参与计算的 A 报价为序号 2、时刻 20、价格 8。
	if rj.Symbol != "A" || rj.Side != Buy || rj.Qty != 1 || rj.Limit != 8 || rj.OrderID != 0 {
		t.Fatalf("拒绝记录须保存本次申请且无订单编号: %+v", rj)
	}
	if !rj.AmtEnabled || rj.AmtLimit != 120 ||
		rj.AmtHolding != 80 || rj.AmtBuyReserved != 40 ||
		rj.AmtTotal != 120 || rj.AmtApplyTotal != 128 {
		t.Fatalf("拒绝记录金额快照错误（应为 80/40/120，申请后 128）: %+v", rj)
	}
	if len(rj.AmtQuoteRefs) != 1 ||
		rj.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 20, Price: 8}) {
		t.Fatalf("拒绝记录须保存 A 的序号 2、时刻 20、价格 8: %+v", rj.AmtQuoteRefs)
	}
	// 日内风险快照反映日号 2：基准与净值 980、亏损 0、上限 20、未限制。
	if !rj.QuoteValid || rj.QuoteSeq != 2 || rj.QuoteMoment != 20 || rj.QuotePrice != 8 {
		t.Fatalf("拒绝记录报价快照应为 seq2(20,8): %+v", rj)
	}
	if rj.RiskDay != 2 || rj.RiskBaseline != 980 || rj.RiskEquity != 980 ||
		rj.RiskLoss != 0 || rj.RiskLimit != 20 || rj.RiskRestrict {
		t.Fatalf("拒绝记录应携带日号 2、基准净值 980、亏损 0、上限 20、未限制的快照: %+v", rj)
	}

	// 拒绝后：5 份买单仍待成交，两类金额与现金占用保留，持仓不变，日号 2 仍未限制。
	if o, _ := e.Order(newID); o.Status != StatusPending || o.Qty != 5 || o.Limit != 8 || o.Remaining() != 5 {
		t.Fatalf("拒绝不得影响已接受的 5 份买单: %+v", o)
	}
	if o, _ := e.Order(canceledID); o.Status != StatusCanceled {
		t.Fatalf("拒绝不得改变上一日已撤销订单的状态: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 40 || e.AvailableCash() != 860 || e.Position("A") != 10 {
		t.Fatalf("拒绝不得改动资金与持仓: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 80 || st.BuyReserved != 40 || st.Total != 120 {
		t.Fatalf("拒绝后金额合计应保持 120: %+v", st)
	}
	if st := e.RiskStatus(); st.Day != 2 || st.Equity != 980 || st.Loss != 0 || st.Restricted {
		t.Fatalf("拒绝后日号 2 仍应亏损 0、未限制: %+v", st)
	}

	// 原有成交及保护记录继续保留各自发生时的数值，跨日与新下单不改写历史。
	var fillRec, trigRec, day1Cancel Record
	haveFill, haveTrig, haveDay1Cancel := false, false, false
	for _, r := range e.Records() {
		switch {
		case r.Kind == RecordFilled && r.TradeID == 1:
			fillRec, haveFill = r, true
		case r.Kind == RecordRiskTriggered:
			trigRec, haveTrig = r, true
		case r.Kind == RecordCanceled && r.OrderID == canceledID:
			day1Cancel, haveDay1Cancel = r, true
		}
	}
	if !haveFill || !haveTrig || !haveDay1Cancel {
		t.Fatalf("必须保留原成交、日号 1 触线与撤单记录: fill=%v trig=%v cancel=%v",
			haveFill, haveTrig, haveDay1Cancel)
	}
	// 成交发生在开日前：成交价 10、数量 10，报价快照停留在 seq1(10,10)，无风险快照。
	if fillRec.TradePrice != 10 || fillRec.Qty != 10 || fillRec.Filled != 10 {
		t.Fatalf("原成交内容必须保留: %+v", fillRec)
	}
	if fillRec.QuoteSeq != 1 || fillRec.QuoteMoment != 10 || fillRec.QuotePrice != 10 {
		t.Fatalf("原成交报价快照必须保留 seq1(10,10): %+v", fillRec)
	}
	if fillRec.RiskDay != 0 || fillRec.RiskRestrict {
		t.Fatalf("开日前的成交记录不得被补写风险快照: %+v", fillRec)
	}
	// 日号 1 触线记录固化基准 1000、净值 980、亏损 20、上限 20 与 seq2(20,8)。
	if trigRec.RiskTrigger != RiskTriggerQuote || trigRec.RiskRefSeq != 2 || trigRec.RiskRefMoment != 20 ||
		trigRec.RiskDay != 1 || trigRec.RiskBaseline != 1000 || trigRec.RiskEquity != 980 ||
		trigRec.RiskLoss != 20 || trigRec.RiskLimit != 20 || !trigRec.RiskRestrict {
		t.Fatalf("日号 1 触线记录必须保留发生时数值: %+v", trigRec)
	}
	if len(trigRec.RiskQuoteRefs) != 1 ||
		trigRec.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 20, Price: 8}) {
		t.Fatalf("日号 1 触线报价定位必须锚定 seq2(20,8): %+v", trigRec.RiskQuoteRefs)
	}
	// 日号 1 的撤单记录固化被取消的 2 份、seq2(20,8) 报价与已限制的风险快照。
	if day1Cancel.Filled != 0 || day1Cancel.Remaining != 2 ||
		day1Cancel.QuoteSeq != 2 || day1Cancel.QuoteMoment != 20 || day1Cancel.QuotePrice != 8 ||
		day1Cancel.RiskDay != 1 || day1Cancel.RiskEquity != 980 || day1Cancel.RiskLoss != 20 ||
		!day1Cancel.RiskRestrict {
		t.Fatalf("日号 1 撤单记录必须保留发生时数值: %+v", day1Cancel)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("日号 2 的下单不得新增任何触线记录")
	}
}

// TestCrossDayCanceledOrderStaysCanceledAndRejectsFill 独立覆盖跨日后的一条边界：
// 日号 2 开启后限制虽解除，但上一日被亏损保护撤销的 2 份买单不能成交（走公开的
// Fill 与查询路径），拒绝不改变资金、持仓与金额状态；同日一张全新买单仍受金额
// 上限约束。
func TestCrossDayCanceledOrderStaysCanceledAndRejectsFill(t *testing.T) {
	e, _, canceledID, recsAfterDay1 := setupCrossDayAmountLimit(t)

	if err := e.StartTradingDay(2, 20); err != nil {
		t.Fatalf("日号 2 合法开日必须成功: %v", err)
	}
	if len(e.Records()) != recsAfterDay1 {
		t.Fatalf("开日不得新增记录，实际新增 %d 条", len(e.Records())-recsAfterDay1)
	}

	// 为上一日已撤销的订单提交一笔新成交编号的成交：必须拒绝，且只新增一条拒绝记录。
	recsBefore := len(e.Records())
	_, err := e.Fill(Trade{TradeID: 2, OrderID: canceledID, Symbol: "A", Side: Buy, Price: 8, Qty: 1})
	if err == nil {
		t.Fatal("已撤销订单的新成交必须被拒绝")
	}
	if !strings.Contains(err.Error(), "已撤销") {
		t.Fatalf("拒绝原因必须说明订单已撤销: %v", err)
	}
	if len(e.Records()) != recsBefore+1 {
		t.Fatalf("成交拒绝只能新增一条记录，实际新增 %d 条", len(e.Records())-recsBefore)
	}
	fr := e.Records()[len(e.Records())-1]
	if fr.Kind != RecordRejected || fr.TradeID != 2 || fr.OrderID != canceledID {
		t.Fatalf("应新增该笔成交的拒绝记录: %+v", fr)
	}
	// 成交拒绝记录同样携带日号 2 的风险快照（未限制），不携带金额上限明细。
	if fr.RiskDay != 2 || fr.RiskBaseline != 980 || fr.RiskEquity != 980 ||
		fr.RiskLoss != 0 || fr.RiskLimit != 20 || fr.RiskRestrict {
		t.Fatalf("成交拒绝记录应携带日号 2 未限制的风险快照: %+v", fr)
	}
	if fr.AmtEnabled {
		t.Fatalf("成交拒绝不应携带金额上限明细: %+v", fr)
	}

	// 拒绝不改变任何账实：现金 900、持仓 10、买单占用 0、金额合计 80。
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.Position("A") != 10 {
		t.Fatalf("成交拒绝不得改动资金与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 120 ||
		st.Holding != 80 || st.BuyReserved != 0 || st.Total != 80 {
		t.Fatalf("金额上限 120 必须保留，合计应只剩持仓金额 80: %+v", st)
	}
}
