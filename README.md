# orb-config-dispatch

把配置项与密钥的键值、目标命名空间、下发版本和校验结果记录成可查询的服务，支持按环境与工作负载查询下发内容并追溯修订与撤销。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `orb-config-dispatch.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

## 配置下发登记与查询

### `POST /v1/config-rollouts`

登记一份配置下发记录。请求体字段：

- `configName`、`targetEnvironment`、`namespace`：非空字符串
- `version`：大于零的十进制整数
- `entries`：至少一项，每项含非空字符串 `key` 与 `value`，`key` 不重复

配置名、目标环境、命名空间与版本构成唯一身份；记录创建后不可修改、不可撤销。成功返回 HTTP 201：

```json
{"configName":"payments","targetEnvironment":"staging","namespace":"billing","version":1,"entries":[{"key":"a","value":"1"}],"digest":"…","revoked":false}
```

- `entries` 按 `key` 升序返回；同一键值集合顺序任意，`digest` 一致（64 位十六进制 SHA-256），任一键或值改变则不同。
- 相同身份重复提交相同内容不新增记录，返回 201 与已有记录；内容不同返回 HTTP 409 与 `ConfigConflictError`。
- JSON 结构错误、字段缺失、条目为空、key 重复、字符串为空或 `version` 非法，返回 HTTP 400 与 `InvalidConfigInputError`。

### `GET /v1/config-rollouts`

只接受非空的 `targetEnvironment`、`namespace`、`configName` 三个查询参数。命中返回 HTTP 200：

```json
{"items":[{"configName":"…","targetEnvironment":"…","namespace":"…","version":1,"entries":[],"digest":"…","revoked":false}]}
```

- `items` 按 `version` 升序，字段与登记响应一致。
- 参数缺失或为空返回 HTTP 400 与 `InvalidConfigInputError`；无命中返回 HTTP 404 与 `ConfigRolloutNotFoundError`。

写入在单个 SQLite 事务内完成，并依赖身份列的唯一约束处理并发提交；重复或冲突不会破坏已有记录，并发查询只读到已提交的内容。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
