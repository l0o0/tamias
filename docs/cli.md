# Tamias CLI 指南

Tamias CLI 复用桌面应用的存储核心，无需 WebView 或图形会话。支持查看状态、管理连接、导入导出配置、执行同步及运行网关。

## 构建

```sh
make cli
```

产物为 `bin/cli/tamias`，Windows 为 `bin/cli/tamias.exe`。构建需要 Go 1.25.0 和支持 CGO 的 C 编译器。源码入口仍为 `cmd/tami`。

## 数据目录

使用 `-data-dir` 指定状态目录。省略时使用操作系统用户配置目录下的 `Tami` 子目录。

同一数据目录仅允许一个进程使用。若与桌面应用共用目录，须先退出桌面应用；关闭窗口会继续后台运行，不等同于退出。

所有参数必须放在子命令前：

```sh
bin/cli/tamias -data-dir ./tamias-state status
bin/cli/tamias -data-dir ./tamias-state diagnostics
```

## 配置与连接

```sh
bin/cli/tamias -data-dir ./tamias-state export > tamias-config.json
bin/cli/tamias -data-dir ./tamias-state -input tamias-config.json import
bin/cli/tamias -data-dir ./tamias-state -input connection.json connection-add
bin/cli/tamias -data-dir ./tamias-state -input credentials.json credentials
```

配置导出不包含秘密凭据。导入会新增连接、停用的同步、备份和迁移任务，以及停止的网关，并清除同步和备份任务的本地路径；重新配置路径、补充凭据并验证后才能使用。

`connection-add` 和 `credentials` 读取 [ConnectionInput](../internal/core/service.go) 定义的 JSON，通用连接字段见 [Config](../internal/storage/storage.go)。创建连接示例：

```json
{
  "name": "WebDAV",
  "kind": "webdav",
  "endpoint": "https://dav.example.com/",
  "username": "your-username",
  "password": "your-password"
}
```

更新凭据时还需提供现有连接的 `id`；可从 `status` 输出中获取。`credentials` 用于替换秘密凭据，不更改远端地址和路径。凭据输入文件应限制访问权限，不要纳入版本控制。

## 同步

```sh
bin/cli/tamias -data-dir ./tamias-state -job JOB_ID preview
bin/cli/tamias -data-dir ./tamias-state -verify-writes -job JOB_ID -token PREVIEW_TOKEN run
```

将 `JOB_ID` 替换为已有任务标识，`PREVIEW_TOKEN` 替换为预览返回的令牌。核对预览中的删除清单后，包含删除的计划还需提供 `-confirm-deletes`。

`-verify-writes` 会对已配置连接创建并清理探测对象，检查远端写入能力。验证失败时命令停止执行。

## 无界面服务

```sh
bin/cli/tamias -data-dir ./tamias-state -verify-writes -gateways GATEWAY_ID serve
```

`serve` 启动任务调度、维护服务及指定的已配置网关，直到收到退出信号。多个网关标识以逗号分隔；未指定 `-gateways` 时仅运行调度和维护服务。

## 凭据存储

默认使用系统凭据库。无桌面环境可通过 `TAMIAS_VAULT_KEY` 环境变量提供 Base64 编码的 32 字节密钥，启用本地 AES-GCM 凭据文件。

密钥不会由应用持久化，后续运行必须提供同一密钥。兼容旧变量 `TAMIOPS_VAULT_KEY` 和 `TAMI_VAULT_KEY`；多个变量同时设置时，按 `TAMIAS_VAULT_KEY`、`TAMIOPS_VAULT_KEY`、`TAMI_VAULT_KEY` 的顺序选择。配置导出不能替代凭据和密钥备份。
