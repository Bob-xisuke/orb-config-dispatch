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

### `POST /v1/config-rollouts`

登记一次配置下发。请求体：

```json
{
  "configName": "billing",
  "targetEnvironment": "prod",
  "namespace": "payments",
  "version": 2,
  "entries": [
    {"key": "alpha", "value": "1"},
    {"key": "zeta", "value": "9"}
  ]
}
```

- `configName`、`targetEnvironment`、`namespace`、`version` 组成唯一身份；`version` 是大于零的十进制整数。
- `entries` 至少一项，`key` 不重复，`key` 与 `value` 都是非空字符串。
- 成功返回 HTTP 201，响应包含上述字段以及 `digest` 与 `revoked`（初始为 `false`）；`entries` 按 `key` 排序返回。
- `digest` 只由键值集合决定：集合相同（与顺序无关）则摘要一致，任一键或值改变则摘要改变。
- 同一唯一身份重复提交相同内容返回 HTTP 201 与已有记录，不新增记录；内容不同返回 HTTP 409 与 `ConfigConflictError`。
- 记录创建后不可修改，暂不提供修改或撤销入口。

### `GET /v1/config-rollouts`

按 `targetEnvironment`、`namespace`、`configName` 三个查询参数检索（都必须非空）。命中返回 HTTP 200：

```json
{
  "items": [
    {
      "configName": "billing",
      "targetEnvironment": "prod",
      "namespace": "payments",
      "version": 2,
      "entries": [{"key": "alpha", "value": "1"}],
      "digest": "...",
      "revoked": false
    }
  ]
}
```

`items` 按 `version` 升序排列。参数缺失或为空返回 HTTP 400 与 `InvalidConfigInputError`；无命中返回 HTTP 404 与 `ConfigRolloutNotFoundError`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

| 场景 | HTTP 状态 | `code` |
|---|---|---|
| 请求体或查询参数非法 | 400 | `InvalidConfigInputError` |
| 查询无命中 | 404 | `ConfigRolloutNotFoundError` |
| 同一身份内容冲突 | 409 | `ConfigConflictError` |
| 存储不可用 | 503 | `storage_unavailable` |
| 未知路径 | 404 | `route_not_found` |
