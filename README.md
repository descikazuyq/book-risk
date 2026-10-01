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
e.Cancel(id)                                  // 撤销只释放未成交部分

e.Cash()            // 现金余额
e.AvailableCash()   // 可用现金
e.ReservedCash()    // 占用现金
e.Position("A")     // 持仓数量
e.Sellable("A")     // 可卖数量
e.Records()         // 接受/拒绝/成交/撤销记录（含当时报价与限额快照）
```

规则要点：只允许先买后卖，不借款、不做空；报价跨号时等待补齐、缺口期间拒绝新买单；
成交编号幂等（同编号同内容返回原结果，不同内容报错）；下调持仓限额会从最后接受的
买单起撤销剩余量。详见 `book/engine.go` 的文档注释。

