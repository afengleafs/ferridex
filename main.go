// ferridex — 一个本地代理,用你已登录的 ChatGPT(Codex)与 Claude 订阅,
// 暴露成 /v1/responses(Codex)和 /v1/messages(Claude),并带一个极简面板。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

const defaultAddr = "127.0.0.1:8788"

func main() {
	log.SetFlags(log.Ltime)

	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("ferridex", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "监听地址")
	noOpen := fs.Bool("no-open", false, "serve 时不自动打开浏览器")
	_ = fs.Parse(args)

	codex := NewCodexProvider()
	claude := NewClaudeProvider()

	switch cmd {
	case "serve":
		runServe(*addr, !*noOpen, codex, claude)
	case "codex":
		runSingle(*addr, codex)
	case "claude":
		runSingle(*addr, claude)
	case "status":
		printStatus(codex, claude)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "ferridex: 未知命令 %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`ferridex — Codex + Claude 订阅本地代理

用法:
  ferridex serve     启动统一网关(Codex + Claude)+ 面板,自动开浏览器(默认命令)
  ferridex codex     只起 Codex 代理(暴露 /v1/responses)
  ferridex claude    只起 Claude 代理(暴露 /v1/messages)
  ferridex status    打印两边登录 / 健康状态

选项:
  -addr <地址>   监听地址(默认 127.0.0.1:8788)
  -no-open       serve 时不自动打开浏览器
`)
}
