# Tamias 发布指南

安装包由 `.github/workflows/release.yml` 在各平台原生构建。发布矩阵为 Windows x64、macOS arm64 / amd64、Linux x64。

## 发布流程

1. 更新版本对应的 `docs/releases/<version>.md`，提交源码、许可证与文档。
2. 在 GitHub Actions 运行 **Release installers**，填写不带 `v` 的版本，例如 `0.2.0-beta.1`；也可推送相应的 `v*` 标签触发。
3. 工作流执行 Go 竞态测试、静态检查、前端类型检查和构建，然后制作安装包。
4. macOS 检查包内容、验证应用的 ad-hoc 签名，并在临时 CI 主机安装；Windows 执行安装、原生启动、版本及卸载检查；Linux 验证 AppImage 中的程序和 WebKit 进程启动。
5. 全部构建与验证通过后创建 **Draft Release**，附四个安装包与 `SHA256SUMS.txt`。核对产物与说明后发布草稿。

草稿重跑仅允许同一源码提交，已发布版本不会被覆盖。应为修改后的已发布内容使用新版本号。

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

产物写入 `dist/`，命名为 `tamias-<版本>-macos-<架构>.pkg`、`tamias-<版本>-windows-amd64-setup.exe` 或 `tamias-<版本>-linux-amd64.AppImage`。GitHub 仓库仍为 `l0o0/tamiops`；已发布的 `0.2.0-beta.1` 安装包和版本说明保留旧 `tamiops` 名称。安装器验证脚本会安装应用，只应在一次性的 CI 环境中执行。打包脚本本身不会安装应用。

## 许可证与运行依赖

`LICENSE` 和 `THIRD_PARTY_NOTICES.txt` 随安装包分发。更新应用依赖后，在 `make setup` 完成的环境执行 `python3 scripts/generate-notices.py`，并提交更新后的声明。AppImage 另在 `usr/share/doc` 保存打包运行库的声明。

macOS 使用系统 WebKit，最低版本与 [Go 1.25 的 macOS 12 要求](https://go.dev/doc/go1.25#darwin)一致。Windows 使用系统 WebView2；Linux AppImage 打包 GTK / WebKit 组件，但仍依赖目标系统的内核、glibc、显示、沙箱工具与凭据服务，详见 [Linux 使用说明](linux.md)。当前构建基线是 Ubuntu 24.04 / glibc 2.39，不承诺在更旧的 Linux 发行版运行。

## 签名

当前未配置发布者证书。macOS 应用仅做 ad-hoc 签名，`.pkg` 未签名且未公证；Windows 安装器未做 Authenticode 签名。正式稳定版应配置合法的发布者证书与公证流程，并在发布前验证结果。
