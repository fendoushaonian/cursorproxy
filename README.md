# cursorproxy

版本 **0.1.0**。在本机提供 OpenAI 兼容接口，把对话转到你已经登录的 Cursor 账号。

默认监听 `http://127.0.0.1:8787`。不设令牌时，启动会自己读取这台机器上 Cursor 的登录态。

## 准备

- 已安装 [Go](https://go.dev/dl/)（1.23 或更高）
- 已安装 `sqlite3` 命令（读取本机登录态时要用）
- 这台电脑上的 Cursor 已经登录；或者你另外准备了 access token

本机 Cursor 的状态库位置：

| 系统 | 路径 |
| --- | --- |
| macOS | `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb` |
| Linux | `~/.config/Cursor/User/globalStorage/state.vscdb` |
| Windows | `%APPDATA%\Cursor\User\globalStorage\state.vscdb` |

机器码从同目录的 `storage.json` 读取。找不到登录态时，程序会直接退出。

## 启动

在仓库根目录：

```bash
go run ./cmd/cursorproxy
```

看到类似下面的日志就说明已经在听端口：

```text
已读取本机 Cursor 登录态
cursorproxy 0.1.0 listening on http://127.0.0.1:8787
```

换端口：

```bash
go run ./cmd/cursorproxy -addr 127.0.0.1:9000
```

也可以先编译再运行：

```bash
go build -o cursorproxy ./cmd/cursorproxy
./cursorproxy
```

## 调用

健康检查：

```bash
curl http://127.0.0.1:8787/healthz
```

返回纯文本 `ok`。

模型列表：

```bash
curl http://127.0.0.1:8787/v1/models
```

默认有 `composer-2.5` 和 `composer-2`。Composer 1 已经下线，不要再请求它。

一次对话：

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"composer-2.5","messages":[{"role":"user","content":"用一句话介绍你自己"}]}'
```

流式输出时加上 `"stream": true`。响应是 `text/event-stream`，最后一行是 `data: [DONE]`。

思考内容在 `reasoning_content` 里，和正文分开。`reasoning_effort` 可以是 `medium` 或 `high`。

带工具的请求使用 OpenAI 的 `tools` / `tool_calls` 形状。`system` 和 `developer` 会当作指令，`tool` 角色的内容会交回模型。

在别的 OpenAI 客户端里，把接口地址指到这里即可，例如：

```text
base_url = http://127.0.0.1:8787/v1
```

这里不校验你填的 API key。上游用的是本机 Cursor 登录态，或者下面配置的令牌。

## 配置

命令行参数优先于环境变量。令牌和机器码都空着时，才读本机 Cursor。

| 参数 | 环境变量 | 默认 | 作用 |
| --- | --- | --- | --- |
| `-addr` | `CURSORPROXY_ADDR` | `127.0.0.1:8787` | 监听地址 |
| `-upstream` | `CURSOR_BASE_URL` | `https://api2.cursor.sh` | Cursor 上游 |
| `-token` | `CURSOR_ACCESS_TOKEN` | 空 | 访问令牌。`userId::jwt` 会自动去掉前缀 |
| `-machine-id` | `CURSOR_MACHINE_ID` | 空 | 机器码。空则用本机的，再没有就从令牌派生 |
| `-models` | `CURSOR_MODELS` | `composer-2.5,composer-2` | 逗号分隔的模型 id |

示例见 `.env.example`。程序不会自动加载 `.env` 文件，需要自己导出，或者直接用命令行参数。不要把真实令牌提交进仓库。

## 版本

`0.1.0` 是这个反代自己的版本。请求头里上报给 Cursor 的客户端版本是另一回事，当前写的是 `3.23.12`。本机安装的 Cursor 更旧时，上游可能拒绝对话，并提示升级客户端或账号没有可用额度。这不是本地接口没起来。
