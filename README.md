# 本地行情、仓位与风险

这是一个在本机运行的 本地行情、仓位与风险。当前基线只提供可编译、可测试的起点。

## 使用

```bash
go test ./...
```

测试通过表示基线包可以加载。后续能力在这个模块上继续增加。

## 本地模拟交易

`book.Engine` 在 `Ready()` 基线之外提供由调用方驱动的单账户、多合约模拟交易：

```go
e, _ := book.NewEngine(1000)                 // 非负初始现金
e.SetMaxPosition("A", 100)                    // 每个合约须先设置非负最大持仓量
e.UpdateQuote("A", book.Quote{Seq: 1, Moment: 10, Price: 10}) // 序号必须连续

id, _ := e.Buy("A", 10, 10)                   // 买单按限价×数量占用现金
e.Fill(book.Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: book.Buy, Price: 9, Qty: 4}) // 调用方提交成交
e.Modify(id, 8, 12)                           // 修改待成交/部分成交订单的总量与限价
e.Cancel(id)                                  // 撤销只释放未成交部分

e.Cash()            // 现金余额
e.AvailableCash()   // 可用现金
e.ReservedCash()    // 占用现金
e.Position("A")     // 持仓数量
e.Sellable("A")     // 可卖数量
e.Records()         // 接受/拒绝/成交/撤销/修改记录（含当时报价与限额快照）
```

规则要点：只允许先买后卖，不借款、不做空；报价跨号时等待补齐、缺口期间拒绝新买单；
成交编号幂等（同编号同内容返回原结果，不同内容报错）；下调持仓限额会从最后接受的
买单起撤销剩余量。详见 `book/engine.go` 的文档注释。

卖单只按**可卖持仓**决定能否接受：合约已有有效报价与持仓限额设置、数量与限价均为
正且委托数量不超过“持仓减去其他有效卖单占用”即可。卖单成交前不占用现金、不产生
金额，因此**不要求整笔“限价 × 委托总量”可用 int64 表示**——整笔委托金额溢出也会
接受，这与把有效卖单 `Modify` 到相同参数的口径一致；买单按整笔限价金额占用现金的
规则不变。接受后订单原样保留总量与限价并占用相应可卖数量，不预先增加现金、扣减持
仓或改变买单现金占用。实际成交按每笔回报的数量与价格结算：单笔成交金额本身超出
int64 时报错并说明金额越界（普通拒绝，不返回 `ErrInt64Overflow`）；卖出所得加回现
金后超出 int64 时整笔拒绝并返回包装 `ErrInt64Overflow` 的错误。两类越界成交都只留
拒绝记录，不改变现金、持仓、订单累计成交量或占用，也不占用成交编号。

### 修改已接受订单

`Modify(orderID, newQty, newLimit)` 按订单编号提交新的委托总量和限价。只能修改待
成交或部分成交订单，合约、方向、订单编号及已成交数量保持不变；新总量必须大于已
成交量，限价必须为正。不存在、已撤销或全部成交的订单与不合法参数都报错并留下带
原因的拒绝记录。取消剩余量继续使用 `Cancel`。

修改以**新占用替换原订单旧占用**，其他合约和订单的占用保留，恰好用满额度允许，
额度不足只拒绝本次修改（绝不撤销其他订单腾额度）：

- 买单剩余量按新限价占用现金；历史成交不重新计价。如买入总量 10、已成交 4，改为
  总量 8、限价 12 后，剩余 4 只占用 48 现金，现金余额与已入账持仓不变。
- 卖单只调整未成交部分占用的可卖数量，不得占用超过持仓的数量。
- 合约持仓限额与账户总持仓金额上限同样按替换口径重算；金额上限拒绝记录会保存
  修改前与申请后合计及参与计算的报价定位。
- 有行情缺口时，买单修改只有在剩余量、现金占用、按现有口径（最新已生效报价，
  等待补齐的报价不参与）的持仓金额占用均**不增加**时才允许；卖单修改不受缺口限制。
- 日内亏损保护继续生效，被保护撤销的订单不能借修改恢复；修改不改变订单原先的
  接受顺序，后续限额下调或报价触发撤单仍沿用已有次序。

每次真实修改追加一条 `RecordModified`，保存修改前后限价、总量、已成交量及当时
报价和限额；对仍有效订单重复提交当前参数成功返回且不新增记录；修改失败只追加一
条拒绝记录，订单、资金、持仓与其他占用保留原值；计算超出 int64 范围时返回包装
`book.ErrInt64Overflow` 的错误。已入账成交的幂等重放仍返回第一次结果（即使订单
后来被修改或撤销），尚未入账的成交按处理时的有效参数判断。

## 单账户日内亏损保护

交易日由调用方用递增的正整数日号显式开启（绝不从行情时刻推导），跨日也只能由调用方
明确发起；未开日时引擎保持上述全部基线行为：

```go
e.StartTradingDay(1, 100)          // 日号 1，日内亏损上限 100；零上限开日立即触线
e.SetLossLimit(50)                 // 当日调整上限；未开日时调用报错
s := e.RiskStatus()                // s.Open / Day / Baseline / Equity / Loss / LossLimit / Restricted
e.StartTradingDay(2, 100)          // 更大日号：重定基准、清除上一日限制（不恢复旧订单）
```

净值口径：现金余额 + 各合约持仓按最新已生效报价计算的市值；未成交买单占用的现金不
重复扣除，跨号等待中的报价不参与估值。日内亏损为基准净值减当前净值，浮盈记零。

亏损达到或超过上限时立即限制增险：拒绝所有合约的新买单，按订单编号从大到小撤销全部
有效买单的未成交部分并释放占用；已入账成交与持仓保留，卖单及其成交、主动撤单不受影响。
限制持续到下一交易日——报价回升、卖出盈利或调高上限都不能解除；下调上限会立刻按当前
亏损检查。每一条真正生效的报价和每一笔首次成功入账的成交都会重新计算风险；缺口补齐后
报价逐条生效，即使中间价格触线、最后价格恢复，限制也保留。

事件记录中新增 `RecordRiskTriggered`（每个交易日首条触线一条），注明由开日、调限额、
报价还是成交引发，关联成交编号或报价序号，并保存计算所用各持仓合约的报价序号、时刻与
价格；触线造成的拒绝与撤销记录也固化当日亏损与上限，后续变化不改写历史。净值或亏损
超出 int64 范围时，对应的开日、调限额、报价或成交整体报错（返回包装
`book.ErrInt64Overflow` 的错误），除拒绝记录外不改变任何业务状态。

### 成交返回与订单状态的时点：一笔成交同时触发风险撤单

一笔买入成交可能在**同一次 `Fill` 调用内**先后发生两件事：成交先完成记账
（结算现金、增加持仓、更新累计成交量），随后引擎重算日内亏损，亏损达到或超过
上限时再撤销该订单（以及其他有效买单）的未成交部分。`Fill` 的返回值与随后的
`Order` 查询分别固定这两个时点，因此完全可能“返回的是部分成交，查到的订单却
已撤销”，两者并不矛盾：

- `FillResult` 固定在**本次成交记账完成、风险撤单之前**：`Filled`、`Remaining`、
  `Status` 只反映这笔成交本身（此处为部分成交），不包含同一调用随后发生的撤单。
- `Order` 查询反映的是**整次操作结束后**的订单：撤单已经生效，状态为已撤销、
  `Remaining()` 为 0，并带有撤销原因。

调用方据此做两个判断：

1. **本次成交是否已记账**——只看 `Fill` 是否返回成功。返回 `nil` 错误即已按成交
   价结算入账；之后订单被风险保护撤销，不会回滚这笔成交，买入的持仓保留。
2. **订单还能不能继续成交**——以重新查询的 `Order` 为准。状态为已撤销或
   `Remaining()` 为 0 后，再为它提交尚未入账的成交只会被拒绝，资金与持仓不变。

下面的例子从创建引擎开始完整走一遍：一张 10 股的买单只成交 5 股，这笔部分成交
恰好把当日亏损做到上限。注意**成交价与估值报价是两个价格**：成交按成交价 10
结算，持仓按最新已生效报价 8 估值；报价从 10 跌到 8 时账户尚无持仓，净值不变，
报价变化本身不会提前触线。

```go
package main

import (
	"fmt"

	"github.com/descikazuyq/book-risk/book"
)

func main() {
	e, _ := book.NewEngine(1000)
	e.SetMaxPosition("A", 100)

	// 连续报价：先有价格 10，随后跌到 8。报价只用于持仓估值，不是成交价。
	e.UpdateQuote("A", book.Quote{Seq: 1, Moment: 10, Price: 10})
	e.StartTradingDay(1, 10) // 明确开启交易日：日号 1，日内亏损上限 10；基准净值 1000
	e.UpdateQuote("A", book.Quote{Seq: 2, Moment: 20, Price: 8})
	// 此时没有持仓，报价下跌不产生亏损：净值仍为 1000，当日亏损 0，不触线。

	// 限价 10 的买单 10 股：按限价×数量冻结 100 现金（现金余额仍为 1000）。
	id, _ := e.Buy("A", 10, 10)

	// 只提交 5 股的成交，成交价 10（买入价不高于限价 10，合法）。
	res, err := e.Fill(book.Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: book.Buy, Price: 10, Qty: 5})
	fmt.Println(err)           // <nil>：这笔成交已经成功记账
	fmt.Println(res.Qty)       // 5  本次成交量
	fmt.Println(res.Filled)    // 5  成交后累计成交量
	fmt.Println(res.Remaining) // 5  成交后、风险撤单前的剩余量
	fmt.Println(res.Status)    // 部分成交

	// res 定格在“成交记账完成、风险撤单之前”；同一调用随后已触发保护撤单，
	// 但不会回填这个结果。订单操作结束后的形态要重新查询：
	o, _ := e.Order(id)
	fmt.Println(o.Status)      // 已撤销
	fmt.Println(o.Filled)      // 5  已成交部分保留
	fmt.Println(o.Remaining()) // 0  已撤销订单没有有效剩余量
	fmt.Println(o.Reason)      // 日内亏损 10 达到或超过上限 10，风险保护撤销全部未成交买单

	// 账实核对：成交按成交价 10 结算，持仓按估值报价 8 估值。
	fmt.Println(e.Cash())         // 950 = 1000 - 5×10（实际成交金额）
	fmt.Println(e.Position("A"))  // 5   已经买入的数量继续留在持仓中
	fmt.Println(e.ReservedCash()) // 0   剩余 5 股的占用在保护撤单时才释放
	s := e.RiskStatus()
	fmt.Println(s.Equity)     // 990 = 950 + 5×8
	fmt.Println(s.Loss)       // 10  = 1000 - 990，亏损恰好等于上限
	fmt.Println(s.Restricted) // true

	// 为已撤销订单再提交一笔“尚未入账”的新成交（新成交编号）会被拒绝，
	// 但此前已经记账的 5 股成交不会被撤回。
	_, err = e.Fill(book.Trade{TradeID: 2, OrderID: id, Symbol: "A", Side: book.Buy, Price: 10, Qty: 1})
	fmt.Println(err)                            // 成交 2 被拒绝: 订单 1 已撤销，不能成交
	fmt.Println(e.Cash(), e.Position("A"))      // 950 5（拒绝不改变任何账实）

	// 同编号、同内容重放：永远返回首次的 FillResult，不把当前的已撤销状态
	// 回填进旧结果，也不再次记账、不新增记录。
	replay, _ := e.Fill(book.Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: book.Buy, Price: 10, Qty: 5})
	fmt.Println(replay == res) // true

	// 这笔操作在买单“接受”记录之后新增的记录，按发生先后排列：
	for _, r := range e.Records()[1:] {
		fmt.Printf("%s filled=%d remaining=%d %s\n", r.Kind, r.Filled, r.Remaining, r.Reason)
	}
	// 成交 filled=5 remaining=5
	// 风险触线 filled=0 remaining=0 日内亏损触线（成交）：基准净值 1000，当前净值 990，亏损 10，亏损上限 10
	// 撤销 filled=5 remaining=5 日内亏损 10 达到或超过上限 10，风险保护撤销全部未成交买单
	// 拒绝 filled=0 remaining=0 订单 1 已撤销，不能成交
}
```

资金与持仓在成交前后的变化，可以逐笔核对：

- **下单后、成交前**：现金余额 1000（买单占用只是冻结，并不扣减）、占用现金
  100（10×10）、可用现金 900、持仓 0、净值 1000、当日亏损 0。
- **5 股成交后**：现金 950 = 1000−5×10，按**成交价**结算；持仓 5；占用现金 0——
  本次成交的 5 股先释放冻结 50，剩余 5 股的冻结 50 要等随后的保护撤单才释放；
  净值 990 = 950 + 5×8，按**最新已生效报价**估值；亏损 1000−990 = 10，恰好等于
  上限。已经买入的 5 股继续留在持仓中，撤单取消的只是尚未成交的另 5 股。

记录的先后关系是**先记成交、再记当日首次触线、最后记剩余量撤销**，上例中依次为：

1. `RecordFilled`：成交编号 1，本次 `Qty=5`、成交后累计 `Filled=5`，其
   `Remaining=5` 表示**成交后仍然有效的剩余量**；
2. `RecordRiskTriggered`：每个交易日只有这一条触线记录，触发来源为成交、关联
   成交编号 1，并固化基准净值 1000、当前净值 990、亏损 10、上限 10 以及估值所用
   报价（合约 A 序号 2、时刻 20、价格 8）；
3. `RecordCanceled`：`Filled=5`，它的 `Remaining=5` 表示**本次被取消的数量**，
   不是“成交后剩余量”——本例两者数值相同，只是因为成交后没有其他动作；语义上
   前者是成交时点的剩余，后者是撤单点上取消的数量。

此后再为该订单提交新成交编号的成交，只追加一条 `RecordRejected`（见上例输出）；
同编号同内容的重放不新增任何记录。

亏损比较是“达到或超过”（`>=`）：上例亏损恰好等于上限 10 也属于触线，剩余量被
撤销；若把上限设为 11，同样这笔成交只造成 10 的亏损（低于上限），`Fill` 同样
返回部分成交，但订单查询仍为部分成交、有效剩余量 5、占用现金 50，剩余量不会
因这笔成交被撤销，还可以继续提交成交。

### 缺口补齐的时点：报价提交成功、价格恢复后，买单为何仍已撤销

行情跨号时，缺失序号之前的报价只能等待。补齐缺失的一条后，引擎把连续的等待
报价按序号**在同一次调用内逐条生效**：每一条都各自重算一次日内亏损。于是完全
可能出现“`UpdateQuote` 返回成功、最新价格已经恢复，先前挂着的买单却已被风险
保护撤销”——撤单发生在这批报价的**中间一条**上，一旦触线，增险限制保留到
下一交易日，批末价格再怎么恢复都不会解除，也不会恢复已撤销的订单。

调用方据此分别做两个判断，二者不要互相替代：

1. **行情是否推进**——看 `UpdateQuote` 返回的新生效条数，再用 `CurrentQuote`
   与 `HasGap` 复核：最新序号推进、缺口消失才表示等待价格已经用上。
2. **订单还能不能成交**——以重新查询的 `Order` 为准：状态为已撤销或
   `Remaining()` 为 0 后，再提交成交只会被拒绝；价格恢复不改变这个结论。

下例从创建引擎开始独立走一遍：账户 1000 现金、合约 A 限额 100、首条报价
seq1@10；一张 10 股限价 10 的买单先成交 5 股，再明确开启日号 1、亏损上限 10
（基准净值 1000）。随后让跨号的 seq3@10（恢复价）先到并等待，再提交缺失的
seq2@8：seq2@8 使净值 990、亏损 10 恰好达到上限，seq3@10 把净值恢复回 1000。
本次调用连续生效两条报价。

```go
package main

import (
	"fmt"

	"github.com/descikazuyq/book-risk/book"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	e, err := book.NewEngine(1000) // 创建账户
	must(err)
	_, err = e.SetMaxPosition("A", 100) // 设置合约持仓限额
	must(err)

	n, err := e.UpdateQuote("A", book.Quote{Seq: 1, Moment: 10, Price: 10}) // 首条报价
	must(err)
	fmt.Println("首条报价新生效条数:", n)

	// 已生效报价的相同内容重复提交：成功返回但 0 条、无缺口、无新事件。
	n, err = e.UpdateQuote("A", book.Quote{Seq: 1, Moment: 10, Price: 10})
	must(err)
	q, _ := e.CurrentQuote("A")
	fmt.Println("已生效报价同内容重提: 条数", n, "缺口", e.HasGap("A"), "最新报价", q.Seq)

	// 10 股限价 10 的买单只成交 5 股：下单冻结 100，成交后剩余 5 股继续冻结 50。
	id, err := e.Buy("A", 10, 10)
	must(err)
	res, err := e.Fill(book.Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: book.Buy, Price: 10, Qty: 5})
	must(err)
	fmt.Println("成交: 本次", res.Qty, "累计", res.Filled, "剩余", res.Remaining, res.Status)

	// 明确开启交易日：日号 1、亏损上限 10；基准净值 = 950 + 5×10 = 1000。
	must(e.StartTradingDay(1, 10))

	// 跨号的 seq3（时刻 30、恢复价 10）先到，只能等待：返回 0 条、出现缺口，
	// 最新报价停在 seq1，净值不提前采用等待价格。
	n, err = e.UpdateQuote("A", book.Quote{Seq: 3, Moment: 30, Price: 10})
	must(err)
	fmt.Println("跨号报价返回条数:", n, "缺口:", e.HasGap("A"))

	// 等待中的同内容报价再次提交：仍是 0 条、仍有缺口，用缺口查询与“已生效
	// 报价重复提交”区分。
	n, err = e.UpdateQuote("A", book.Quote{Seq: 3, Moment: 30, Price: 10})
	must(err)
	fmt.Println("等待报价同内容重提: 条数", n, "缺口", e.HasGap("A"))

	// 相同序号但时刻或价格不同：冲突错误，原报价不被替换。
	_, err = e.UpdateQuote("A", book.Quote{Seq: 3, Moment: 30, Price: 9})
	fmt.Println("等待序号改价:", err)

	// ---- 补齐前：最新报价、缺口、风险状态与订单状态 ----
	q, _ = e.CurrentQuote("A")
	s := e.RiskStatus()
	o, _ := e.Order(id)
	fmt.Println("补齐前: 最新报价", q.Seq, q.Moment, q.Price, "缺口", e.HasGap("A"),
		"净值", s.Equity, "亏损", s.Loss, "限制", s.Restricted)
	fmt.Println("补齐前: 订单", o.Status, "累计成交", o.Filled, "剩余", o.Remaining(),
		"现金", e.Cash(), "占用", e.ReservedCash(), "持仓", e.Position("A"))

	// 提交缺失的 seq2（时刻 20、价格 8）：链 seq2@8 → seq3@10 在本次调用中
	// 连续生效两条。seq2@8 恰好触线，seq3@10 恢复到开日估值水平。
	n, err = e.UpdateQuote("A", book.Quote{Seq: 2, Moment: 20, Price: 8})
	must(err)
	fmt.Println("补齐返回新生效条数:", n) // 只统计新生效报价，不统计同次调用的触线与撤单

	// ---- 补齐后：最新报价已恢复，限制仍在、未成交部分已撤销 ----
	q, _ = e.CurrentQuote("A")
	s = e.RiskStatus()
	fmt.Println("补齐后: 最新报价", q.Seq, q.Moment, q.Price, "缺口", e.HasGap("A"),
		"净值", s.Equity, "亏损", s.Loss, "限制", s.Restricted)
	o, _ = e.Order(id)
	fmt.Println("补齐后: 订单", o.Status, "累计成交", o.Filled, "剩余", o.Remaining(),
		"现金", e.Cash(), "占用", e.ReservedCash(), "持仓", e.Position("A"))
	fmt.Println("撤单原因:", o.Reason)

	// 当日增险限制仍然保留：价格恢复后新买单照样被拒绝。
	_, err = e.Buy("A", 1, 10)
	fmt.Println("恢复后新买单:", err)

	// 事件记录：当日首次触线记录先于撤销记录，且都固化在中间价 seq2@8。
	for _, r := range e.Records() {
		switch r.Kind {
		case book.RecordRiskTriggered:
			fmt.Printf("记录: %s 来源=%s 报价序号=%d 时刻=%d 价格=%d 净值=%d 亏损=%d 上限=%d\n",
				r.Kind, r.RiskTrigger, r.RiskRefSeq, r.RiskRefMoment, r.RiskQuoteRefs[0].Price,
				r.RiskEquity, r.RiskLoss, r.RiskLimit)
		case book.RecordCanceled:
			fmt.Printf("记录: %s 订单=%d 累计成交=%d 本次取消=%d 快照报价=%d@%d 快照净值=%d 快照亏损=%d\n",
				r.Kind, r.OrderID, r.Filled, r.Remaining, r.QuoteSeq, r.QuotePrice,
				r.RiskEquity, r.RiskLoss)
		}
	}
}
```

输出：

```text
首条报价新生效条数: 1
已生效报价同内容重提: 条数 0 缺口 false 最新报价 1
成交: 本次 5 累计 5 剩余 5 部分成交
跨号报价返回条数: 0 缺口: true
等待报价同内容重提: 条数 0 缺口 true
等待序号改价: 等待中的报价序号 3 内容冲突: 已有 (时刻=30, 价格=10)，新值 (时刻=30, 价格=9)
补齐前: 最新报价 1 10 10 缺口 true 净值 1000 亏损 0 限制 false
补齐前: 订单 部分成交 累计成交 5 剩余 5 现金 950 占用 50 持仓 5
补齐返回新生效条数: 2
补齐后: 最新报价 3 30 10 缺口 false 净值 1000 亏损 0 限制 true
补齐后: 订单 已撤销 累计成交 5 剩余 0 现金 950 占用 0 持仓 5
撤单原因: 日内亏损 10 达到或超过上限 10，风险保护撤销全部未成交买单
恢复后新买单: 买入 A 委托被拒绝: 交易日 1 日内亏损保护已触发（亏损上限 10），拒绝所有新买单
记录: 风险触线 来源=报价 报价序号=2 时刻=20 价格=8 净值=990 亏损=10 上限=10
记录: 撤销 订单=1 累计成交=5 本次取消=5 快照报价=2@8 快照净值=990 快照亏损=10
```

资金与持仓可以逐笔对上例数字核对：

- **下单时**：冻结限价×数量 10×10=100，现金余额仍为 1000。
- **5 股成交后**：按**成交价**扣 5×10=50，现金 950、持仓 5、订单累计成交 5；
  未成交的另 5 股继续冻结 5×10=50（占用 50）。
- **触线撤单时**：只取消未成交的 5 股，释放它的冻结 50（占用 50→0）；现金
  余额 950、已买入持仓 5、累计成交量 5 全部保留。seq3 把价格恢复到 10 只影响
  估值，**不会退回成交、不会补回持仓，也不会恢复已撤销订单**。

历史记录与调用结束后的现状之所以不同，是因为记录固化的是**各自发生时点**的
快照，而最新报价与当前亏损反映的是**整次调用结束后**的状态：

1. `RecordRiskTriggered` 是当日唯一一条触线记录，先于撤销记录写入；它锚定
   真正触线的中间报价——序号 2、模拟时刻 20、价格 8，以及当时的净值 990、
   亏损 10、上限 10。
2. 随后的 `RecordCanceled` 保存同一时点的报价快照（seq2@8）与亏损 10；
   `Filled=5` 是保留的累计成交，`Remaining=5` 是**本次被取消的数量**。
3. 链继续推进到 seq3@10 后，`CurrentQuote` 指向序号 3、当前亏损按恢复价
   重算为 0，但触线记录与撤单原因不会被回头改写——读者正是靠这些历史值得知
   “限制是被 seq2@8 的 10 元亏损触发的”，而不是被批末价格触发。

`UpdateQuote` 返回的条数只统计**本次新生效的报价**，同一次调用里发生的触线、
撤单与拒绝都不计入。返回 `0` 条且错误为 `nil` 时，需要结合 `CurrentQuote` 与
`HasGap` 区分两种情形：报价正在等待（有缺口，如跨号的 seq3），或相同内容的
报价重复提交（已生效或等待中的同内容重提，均无新事件）。相同序号但时刻或
价格不同则返回冲突错误，原报价不被替换（无论它已生效还是在等待）。

开启亏损保护后，补齐链在生效前会先按“前一条处理后的状态”逐步预检整条链：
**任一步净值或亏损计算超出 int64 范围，整批拒绝**——返回包装
`book.ErrInt64Overflow` 的错误和 0 条生效，只增加一条指出出错序号的拒绝记录；
已生效报价、原先等待的报价、订单（含本拟触发的撤单）与现金/持仓占用全部保留
提交前值，修正报价后可用同一批序号重新补齐。

```go
package main

import (
	"errors"
	"fmt"
	"math"

	"github.com/descikazuyq/book-risk/book"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	// 现金贴近 int64 上界；买入 1 股成交价 100 后，现金 MaxInt64-200、持仓 1。
	e, err := book.NewEngine(math.MaxInt64 - 100)
	must(err)
	_, err = e.SetMaxPosition("A", 10)
	must(err)
	_, err = e.UpdateQuote("A", book.Quote{Seq: 1, Moment: 10, Price: 100})
	must(err)
	id, err := e.Buy("A", 1, 100)
	must(err)
	_, err = e.Fill(book.Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: book.Buy, Price: 100, Qty: 1})
	must(err)
	must(e.StartTradingDay(1, 10)) // 基准净值 = MaxInt64-100
	bid, err := e.Buy("A", 1, 1)   // 开日后的待成交买单，占用现金 1
	must(err)

	// 跨号 seq3 是天价：1 股 × MaxInt64 再加现金必然溢出净值；它先等待。
	_, err = e.UpdateQuote("A", book.Quote{Seq: 3, Moment: 30, Price: math.MaxInt64})
	must(err)
	recsBefore := len(e.Records())

	// 补齐 seq2@100：预检走到 seq3 时发现净值溢出，整批拒绝、0 条生效。
	n, err := e.UpdateQuote("A", book.Quote{Seq: 2, Moment: 20, Price: 100})
	fmt.Println("溢出补齐: 条数", n, "ErrInt64Overflow =", errors.Is(err, book.ErrInt64Overflow))
	fmt.Println("错误:", err)

	// 已生效报价、等待报价、风险状态、订单与占用全部保留提交前值。
	q, _ := e.CurrentQuote("A")
	st := e.RiskStatus()
	o, _ := e.Order(bid)
	fmt.Println("最新报价", q.Seq, q.Price, "缺口", e.HasGap("A"), "限制", st.Restricted, "净值", st.Equity)
	fmt.Println("订单", o.Status, "剩余", o.Remaining(), "现金", e.Cash(), "占用", e.ReservedCash(), "持仓", e.Position("A"))
	rj := e.Records()[recsBefore]
	fmt.Println("仅新增记录数:", len(e.Records())-recsBefore, "类型:", rj.Kind, "原因:", rj.Reason)
}
```

输出：

```text
溢出补齐: 条数 0 ErrInt64Overflow = true
错误: 报价序号 3: 净值、亏损或持仓金额超出 int64 范围
最新报价 1 100 缺口 true 限制 false 净值 9223372036854775707
订单 待成交 剩余 1 现金 9223372036854775607 占用 1 持仓 1
仅新增记录数: 1 类型: 拒绝 原因: 报价序号 3 生效将使净值或亏损超出 int64 范围，整批拒绝
```

## 账户总持仓金额上限

调用方可显式设置账户级（跨全部合约合计）的总持仓金额上限；未设置时保持基线行为，
设置后跨交易日保留，不随交易日重置：

```go
e.SetPositionAmountLimit(1000)        // 非负 int64；负值报错并保留原设置
s := e.PositionAmountStatus()         // s.Enabled / Limit / Holding / BuyReserved / Total
// s.Holding     = 各合约持仓数量 × 最新已生效报价之和
// s.BuyReserved = 各有效买单 剩余数量 × max(限价, 最新报价) 之和
// s.Total       = 两者合计；未启用时除 Enabled 外均为零
```

口径要点：卖单占用的持仓在成交前仍计入持仓金额，不能提前抵减；同一笔数量不会在
持仓与买单两处重复计算。新买单只有在“申请后合计 ≤ 上限”时才接受（恰好等于允许），
且现金、合约持仓限额与行情缺口等原有规则仍须全部满足；零上限禁止任何买单。部分买入
成交后按实际持仓与剩余买单重新计算金额，现金仍按实际成交价结算；卖出成交减少持仓
金额；主动撤销买单只释放其剩余量对应的额度。

首次设置、调整上限以及每条报价真正生效后立即收敛超限：跨合约按订单编号从大到小
撤销买单的全部剩余量，直到合计不再超限；若仅持仓金额本身已超限，则撤掉所有剩余
买单但不自动卖出，正常卖单及其成交仍允许。与日内亏损保护不同，本项不做整日锁定：
报价回落、卖出或上调上限后，只要申请后符合额度便可再次买入；已撤销订单不恢复。
若同一条报价同时触发两项保护，先按原有亏损规则处理并保留其撤单原因；亏损保护的
整日锁定仍然有效。

等待补齐的报价不参与计算；一次补齐多条报价时按前一条处理后的状态逐条判断，中间
报价造成的撤单即使最后价格回落也保留；相同内容的报价与成交重放不新增业务效果或
记录。因本上限产生的拒绝与撤销记录会固化具体原因、判断时的两类金额与上限（拒绝
另存申请后合计），以及参与计算的各合约报价序号、时刻、价格；后续操作或查询副本
不能改写历史。设置、下单或报价提交时若金额乘积或合计超出 int64 范围，对应调用
整体报错（返回包装 `book.ErrInt64Overflow` 的错误），只增加一条拒绝记录；补齐
链中任一步溢出时整批不生效，已生效报价、撤单、占用变化都不留下，原先等待的报价
仍然保留。

