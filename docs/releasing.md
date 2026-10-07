# 小花鼠（Tamias）发布指南

安装包由 `.github/workflows/release.yml` 在各平台原生构建。发布矩阵为 Windows x64、macOS arm64 / amd64、Linux x64。

## 自动发布

推荐通过版本标签发布。先将源码与本工作流提交到仓库，再推送一个新的 `v<版本>` 标签：

```sh
git tag v0.2.0-beta.2
git push origin v0.2.0-beta.2
```

`Release installers` 会自动完成构建、安装验证、上传附件和发布，无需手动上传安装包。带预发布后缀的版本（如 `-beta.2`）会标为 Pre-release；其余版本标为正式 Release。上方版本号仅为示例，每次发布应使用新的版本号。

也支持以下入口：

- 在 GitHub Releases 页面发布正式版本或预发布版本，选择 `v<版本>` 标签。工作流收到 `release.published` 事件后，为该 Release 添加安装包，并保留用户填写的标题和说明。
- 在 Actions → **Release installers** → **Run workflow** 输入不带 `v` 的版本号，重新构建并发布已存在的标签。没有标签时提前失败，不会把当前分支误当作该版本源码。

标签必须包含本工作流及打包脚本。版本采用 `主版本.次版本.修订号`，可加 `-beta.2` 等预发布后缀，不使用 `+` 构建元数据。可以提前填写 `docs/releases/<版本>.md`；自动创建 Release 时优先使用该文件，没有时生成 GitHub 版本说明。

### 构建与安装验证

1. 校验版本并解析标签，将四个平台锁定到同一个源码提交；上传前再次确认远端标签没有移动。
2. 在各平台运行 Go 竞态测试、静态检查、前端类型检查和生产构建。
3. macOS 验证包内容和 ad-hoc 签名，在临时 CI 主机安装并核对版本；Windows 执行安装、原生窗口启动、版本及卸载检查；Linux 验证 AppImage 和 WebKit 进程启动。
4. 四个平台全部成功后，上传以下安装包与 `SHA256SUMS.txt`。任一平台失败不会进入上传步骤，可在 Actions 重跑失败任务。

| 系统 | 安装包 |
| --- | --- |
| Windows x64 | `tamias-<版本>-windows-amd64-setup.exe` |
| macOS Apple Silicon | `tamias-<版本>-macos-arm64.pkg` |
| macOS Intel | `tamias-<版本>-macos-amd64.pkg` |
| Linux x64 | `tamias-<版本>-linux-amd64.AppImage` |

自动创建的新 Release 会先保持草稿，附件完整上传后再公开。已公开 Release 的重跑保留已有安装包，只补齐缺失文件；校验文件对应最终附件，校验失败不会静默覆盖。变更已发布版本的内容应使用新标签。

启用 GitHub 不可变发布时，必须使用标签触发的流程，让安装包先上传再公开；已公开且附件不完整的不可变 Release 无法补传，需要新版本。参见 [GitHub Release 触发规则](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#release) 和 [不可变 Release](https://cli.github.com/manual/gh_release_create)。

使用 GitHub 自动提供的 `GITHUB_TOKEN`，无需额外配置上传凭据；只有发布任务申请 `contents: write`，构建任务保持只读。相同版本的执行串行排队，防止重复触发时同时上传。

## 工作流回归检查

```sh
bash -n scripts/publish-release.sh
python3 -B -m unittest discover -s scripts/tests -v
```

发布回归测试使用模拟 Git/GitHub 命令，不创建真实 Release、不上传安装包。普通 CI 会运行这些测试。跨平台安装测试只在一次性的 GitHub runner 上执行，不应在日常使用的电脑上运行。

## 本机打包

各平台先按照 README 准备本机编译环境。Go 版本由 `go.mod` 固定，Wails 版本由依赖锁定，发布版本由 `VERSION` 注入。

```sh
VERSION=0.2.0-beta.1 make build
# macOS：ARCH 必须与已构建应用一致
VERSION=0.2.0-beta.1 ARCH=arm64 sh scripts/package-macos.sh
# Linux：在 Ubuntu 24.04 x64 上运行
VERSION=0.2.0-beta.1 ARCH=amd64 bash scripts/package-linux.sh
```

Windows 在 PowerShell 中设置版本与架构后使用 Inno Setup 6：

```powershell
$env:VERSION = '0.2.0-beta.1'
$env:ARCH = 'amd64'
bash scripts/build-desktop.sh
./scripts/package-windows.ps1
```

产物写入 `dist/`，命名为 `tamias-<版本>-macos-<架构>.pkg`、`tamias-<版本>-windows-amd64-setup.exe` 或 `tamias-<版本>-linux-amd64.AppImage`。GitHub 仓库为 `l0o0/tamias`；已发布的 `0.2.0-beta.1` 安装包和版本说明保留旧 `tamiops` 名称。安装器验证脚本会安装应用，只应在一次性的 CI 环境中执行。打包脚本本身不会安装应用。

## 许可证与运行依赖

`LICENSE` 和 `THIRD_PARTY_NOTICES.txt` 随安装包分发。更新应用依赖后，在 `make setup` 完成的环境执行 `python3 scripts/generate-notices.py`，并提交更新后的声明。AppImage 另在 `usr/share/doc` 保存打包运行库的声明。

macOS 使用系统 WebKit，最低版本与 [Go 1.25 的 macOS 12 要求](https://go.dev/doc/go1.25#darwin)一致。Windows 使用系统 WebView2；Linux AppImage 打包 GTK / WebKit 组件，但仍依赖目标系统的内核、glibc、显示、沙箱工具与凭据服务，详见 [Linux 使用说明](linux.md)。当前构建基线是 Ubuntu 24.04 / glibc 2.39，不承诺在更旧的 Linux 发行版运行。

## 签名

当前未配置发布者证书。macOS 应用仅做 ad-hoc 签名，`.pkg` 未签名且未公证；Windows 安装器未做 Authenticode 签名。正式稳定版应配置合法的发布者证书与公证流程，并在发布前验证结果。
