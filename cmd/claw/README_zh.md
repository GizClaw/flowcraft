# Claw

Claw 是一个本地 workspace 运行器，用来运行基于 graph 的对话 agent。CLI 可以从内置配置创建 workspace、打开 TUI、提供 debug API，也可以跑脚本化测试。Go SDK 可以把同一个运行时嵌入到其它程序里。

## CLI 用法

开发时可以在 `cmd/claw` 目录运行：

```sh
go run . help
```

也可以先编译成一个本地 binary：

```sh
go build -o claw .
./claw help
```

### 配置

Claw 启动时会把内置配置同步到用户配置目录。可以这样查看可用配置：

```sh
claw config raid list
claw config persona list
claw config test list
```

配置分为 `raid`、`persona` 和 `test`。像 `journey`、`murder_mystery` 这样的名字可以传给接受 `<name|path>` 的命令；也可以直接传 YAML 文件路径。

### Workspace

从配置创建 workspace：

```sh
claw workspace create --config journey --workspace ./workspace
```

查看 workspace：

```sh
claw workspace inspect --workspace ./workspace
```

workspace 里会有 `config.yaml`，以及运行时产生的 history、memory、graph state 等数据。`config.yaml` 控制 agent graph、模型别名、history 和 memory 行为。

### TUI

新建一个 TUI 会话，先选择 raid 配置：

```sh
claw tui new
```

从已有 workspace 恢复：

```sh
claw tui resume
```

TUI 是左中右布局：左侧 recall，中间 chat，右侧 workspace/debug 信息。`tab` 切换焦点，`enter` 提交，`q` 或 `esc` 退出。

### Debug API

为一个 workspace 启动 debug HTTP API：

```sh
claw serve --workspace ./workspace --addr 127.0.0.1:8787
```

目前提供这些 endpoint：

```text
GET  /debug
GET  /debug/workspace
GET  /debug/history?context_id=__default__
GET  /debug/memory
POST /debug/recall
```

recall 请求示例：

```sh
curl -s http://127.0.0.1:8787/debug/recall \
  -H 'content-type: application/json' \
  -d '{"text":"故事讲到哪里了？","top_k":5}'
```

### 测试

运行单个内置脚本测试：

```sh
claw test -test journey/opening_continue --timeout 2m
```

运行 raid/persona 自动对话：

```sh
claw test-auto --raid journey --persona boy_14_Tom --timeout 5m
```

测试输出会写到 `.out/`，里面包含 workspace 快照、chat log 和 stats 文件。

## SDK 用法

在 Go 程序里可以直接使用 `sdkx/claw`，底层 workspace 使用 SDK 的 workspace 抽象。

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/claw"
)

func main() {
	ws, err := workspace.NewLocalWorkspace("./workspace")
	if err != nil {
		panic(err)
	}

	app, err := claw.New(ws)
	if err != nil {
		panic(err)
	}
	defer app.Close()

	resp, err := app.RoundTrip(claw.Request{
		Context: context.Background(),
		Text:    "继续讲故事",
	})
	if err != nil {
		panic(err)
	}

	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			panic(err)
		}
		switch ev.Type {
		case claw.EventToken:
			fmt.Print(ev.Content)
		case claw.EventStatus:
			fmt.Printf("\n[%s]\n", ev.Content)
		case claw.EventError:
			panic(ev.Err)
		}
	}
}
```

### Workspace 约定

`claw.New` 需要 workspace 里存在 `config.yaml`。CLI 创建的 workspace 会自动包含这个文件。如果是在自己的 app 里嵌入 Claw，需要先创建或复制配置，再构造 runtime。

### 工具调用

工具 schema 写在配置里，真正执行逻辑在 Go 里注册。下面这个短片段省略了 import：

```go
app.Handle("play_music", func(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return `{"ok":true}`, nil
})

app.HandleDefault(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return `{"ok":true}`, nil
})
```

### Debug Handler

也可以把同一套 debug API 嵌进自己的 HTTP 服务：

```go
handler := claw.NewDebugHTTPHandler(app)
http.ListenAndServe("127.0.0.1:8787", handler)
```

### History 和说话人信息

启用 history 后，Claw 会把对用户可见的 assistant 输出写到 workspace 的 history 目录。可见 node 信息会保存成 XML message part：

```json
{"type":"text/xml","text":"<node id=\"storyteller_origin\" />"}
```

assistant 输出也可以用 speaker 指令开头：

```xml
<speaker name="沈知秋" />我那晚确实去过书房。
```

Claw 会把 speaker 保存成 `text/xml` part，正文继续作为普通 text part 保存。后续轮次里，这些 XML part 会作为正常文本上下文发给模型，让模型能看到历史里的说话边界。
