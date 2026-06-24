// ferridex — 一个本地代理,用你已登录的 ChatGPT(Codex)、Claude 与 Cursor 订阅,
// 暴露成 /v1/responses(Codex)、/v1/messages(Claude)、Connect-RPC(Cursor),并带一个极简面板。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"ferridex/internal/provider"
	"ferridex/internal/server"
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
	autoPort := fs.Bool("auto-port", false, "端口被占用时自动尝试后续端口")
	lan := fs.Bool("lan", false, "绑定 0.0.0.0,向局域网开放 AI 接口(需密钥);面板仍仅限本机")
	newKey := fs.Bool("new-key", false, "配合 -lan:强制重新生成局域网密钥 lan_key")
	_ = fs.Parse(args)

	codex := provider.NewCodexProvider()
	claude := provider.NewClaudeProvider()
	cursor := provider.NewCursorProvider()

	switch cmd {
	case "serve":
		server.RunServe(*addr, !*noOpen, *lan, *newKey, *autoPort, codex, claude, cursor)
	case "codex":
		server.RunSingle(*addr, *autoPort, codex)
	case "claude":
		server.RunSingle(*addr, *autoPort, claude)
	case "cursor":
		server.RunSingle(*addr, *autoPort, cursor)
	case "status":
		server.PrintStatus(codex, claude, cursor)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "ferridex: 未知命令 %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`ferridex — Codex + Claude + Cursor 订阅本地代理

用法:
  ferridex serve     启动统一网关(Codex + Claude + Cursor)+ 面板,自动开浏览器(默认命令)
  ferridex codex     只起 Codex 代理(暴露 /v1/responses)
  ferridex claude    只起 Claude 代理(暴露 /v1/messages)
  ferridex cursor    只起 Cursor 代理(Connect-RPC 透明反代)
  ferridex status    打印各 provider 登录 / 健康状态

选项:
  -addr <地址>   监听地址(默认 127.0.0.1:8788)
  -no-open       serve 时不自动打开浏览器
  -auto-port     端口被占用时自动尝试后续端口
  -lan           绑定 0.0.0.0,向同一局域网开放 AI 接口(自动生成密钥);
                 Web 面板与 SSH 隧道控制仍仅限本机
  -new-key       配合 -lan:启动时强制重新生成局域网密钥(旧密钥立即失效)
`)
}
