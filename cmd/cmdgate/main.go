// cmdgate 是 Agent 与特权命令之间的一道门
//
// 它持有凭证，按策略和人的决定代为执行已注册的脚本，调用者只拿到执行结果
package main

import (
	"os"

	// 登记 Telegram 渠道
	_ "cmdgate/internal/channel/telegram"
	"cmdgate/internal/cli"
)

// main 运行命令行并返回其退出码
func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
