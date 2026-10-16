package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	mcp "trpc.group/trpc-go/trpc-mcp-go"
)

// MCP 模式把同一套笔记工具原样暴露为 MCP server（streamable HTTP）：
// 工具名、描述、入参结构、执行语义与 function 模式完全一致，仅传输
// 协议不同。Agent 侧经 mcp.ToolSet 消费（见 app.go），用于验证 ack
// 许可网关对 MCP 工具与 function 工具同等生效。

// notesMCPServer 是已启动的笔记 MCP 工具服务，独立监听使测试无需
// 预知主服务端口，外部 MCP 客户端也可直连观察。
type notesMCPServer struct {
	URL    string       // 客户端连接地址（POST /mcp）
	server *http.Server // 承载 MCP handler 的 HTTP 服务
}

// Close 停止 MCP 工具服务。
func (s *notesMCPServer) Close() error {
	return s.server.Close()
}

// startNotesMCP 注册笔记工具并在 127.0.0.1 随机端口启动 streamable
// HTTP 服务，返回连接地址。
func startNotesMCP(n *Notepad) (*notesMCPServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen mcp server: %w", err)
	}

	srv := mcp.NewServer("notes-tools", "1.0.0", mcp.WithServerLogger(slogLogger{}))
	srv.RegisterTool(readNoteTool(), n.readHandler)
	srv.RegisterTool(writeNoteTool(), n.writeHandler)
	srv.RegisterTool(deleteNoteTool(), n.deleteHandler)
	srv.RegisterTool(dropAllNotesTool(), n.dropHandler)

	s := &notesMCPServer{
		URL:    "http://" + ln.Addr().String() + "/mcp",
		server: &http.Server{Handler: srv.Handler()},
	}
	go func() { _ = s.server.Serve(ln) }()
	return s, nil
}

// 四个工具的 MCP 声明：名称与描述复制自 tools.go，入参 schema 与
// function 模式（结构体推导出的扁平对象）对齐，均不标注 required。

func readNoteTool() *mcp.Tool {
	return mcp.NewTool("read_note",
		mcp.WithDescription("读取一条笔记的内容。参数 note_id 为笔记 ID（如 n1、n2、n3）。"),
		mcp.WithString("note_id"),
	)
}

func writeNoteTool() *mcp.Tool {
	return mcp.NewTool("write_note",
		mcp.WithDescription("写入一条笔记（新建或覆盖）。参数 note_id 为笔记 ID，content 为内容。"),
		mcp.WithString("note_id"),
		mcp.WithString("content"),
	)
}

func deleteNoteTool() *mcp.Tool {
	return mcp.NewTool("delete_note",
		mcp.WithDescription("永久删除一条笔记，不可恢复。参数 note_id 为笔记 ID（如 n1、n2、n3）。"),
		mcp.WithString("note_id"),
	)
}

func dropAllNotesTool() *mcp.Tool {
	return mcp.NewTool("drop_all_notes",
		mcp.WithDescription("清空全部笔记。该工具被许可层级禁用，用于演示 deny 不可降级。"),
	)
}

// 处理函数与 tools.go 的方法一一对应：入参经 JSON 往返绑定到同一套
// 结构体，结果以与 function 模式相同的 map 序列化为文本内容返回。

func (n *Notepad) readHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in noteInput
	if err := bindArgs(req, &in); err != nil {
		return nil, err
	}
	return mapResult(n.read(ctx, in))
}

func (n *Notepad) writeHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in writeInput
	if err := bindArgs(req, &in); err != nil {
		return nil, err
	}
	return mapResult(n.write(ctx, in))
}

func (n *Notepad) deleteHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in noteInput
	if err := bindArgs(req, &in); err != nil {
		return nil, err
	}
	return mapResult(n.delete(ctx, in))
}

func (n *Notepad) dropHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mapResult(n.drop(ctx, struct{}{}))
}

// bindArgs 把 MCP 调用参数经 JSON 往返绑定为工具入参，使 MCP 模式与
// function 模式共享同一套入参结构体。
func bindArgs(req *mcp.CallToolRequest, in any) error {
	b, err := json.Marshal(req.Params.Arguments)
	if err != nil {
		return fmt.Errorf("encode tool arguments: %w", err)
	}
	if err := json.Unmarshal(b, in); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	return nil
}

// mapResult 把与 function 模式一致的结果序列化为 MCP 文本内容，模型
// 看到的信息内容两种模式相同（外层包裹为 MCP 内容数组）。
func mapResult(res map[string]any, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	return mcp.NewTextResult(string(b)), nil
}

// slogLogger 把 trpc-mcp-go 的日志接口适配到示例统一使用的 slog。
type slogLogger struct{}

func (slogLogger) Debug(args ...any)         { slog.Debug(fmt.Sprint(args...)) }
func (slogLogger) Debugf(f string, a ...any) { slog.Debug(fmt.Sprintf(f, a...)) }
func (slogLogger) Info(args ...any)          { slog.Info(fmt.Sprint(args...)) }
func (slogLogger) Infof(f string, a ...any)  { slog.Info(fmt.Sprintf(f, a...)) }
func (slogLogger) Warn(args ...any)          { slog.Warn(fmt.Sprint(args...)) }
func (slogLogger) Warnf(f string, a ...any)  { slog.Warn(fmt.Sprintf(f, a...)) }
func (slogLogger) Error(args ...any)         { slog.Error(fmt.Sprint(args...)) }
func (slogLogger) Errorf(f string, a ...any) { slog.Error(fmt.Sprintf(f, a...)) }
func (slogLogger) Fatal(args ...any)         { slog.Error(fmt.Sprint(args...)) }
func (slogLogger) Fatalf(f string, a ...any) { slog.Error(fmt.Sprintf(f, a...)) }
