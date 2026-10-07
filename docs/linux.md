# Tamias Linux AppImage

下载 x64 AppImage，赋予执行权限后启动：

以下示例使用 `0.2.0-beta.3`。其他版本请替换为对应的安装包文件名。

```sh
chmod +x tamias-0.2.0-beta.3-linux-amd64.AppImage
./tamias-0.2.0-beta.3-linux-amd64.AppImage
```

当前构建要求 glibc 2.39+、X11 或 XWayland 图形会话、D-Bus、`bubblewrap`、`xdg-dbus-proxy`，以及允许 WebKit 沙箱使用的非特权用户命名空间。凭据存储需要 Secret Service（例如 GNOME Keyring）。GTK、WebKit 及其进程组件包含在 AppImage 内；系统服务和沙箱工具由发行版提供。Ubuntu 缺少沙箱工具时可安装：

```sh
sudo apt install bubblewrap xdg-dbus-proxy
```

从 `0.2.0-beta.3` 起，AppImage 内含 Fcitx 5 的 GTK 4 输入法模块。应用使用桌面会话现有的输入法进程和设置，不会启动或配置输入法。若 GTK 4 程序尚未选择 Fcitx，可按桌面环境设置 GTK 4 的输入法模块，例如在 `~/.config/gtk-4.0/settings.ini` 中配置：

```ini
[Settings]
gtk-im-module=fcitx
```

GTK 4 的输入法模块选择也会受当前进程的 `GTK_IM_MODULE` 环境变量影响；AppImage 保留该变量及桌面设置，不会替用户改选输入法。更多环境说明见 [Fcitx 5 官方 GTK 设置指南](https://fcitx-im.org/wiki/Using_Fcitx_5_on_Wayland)。

输入法兼容性检查使用 Ubuntu 24.04 构建同一个 AppImage，再分别在 Ubuntu 24.04 与 26.04 的 X11 测试环境中验证包内 GTK 4、Fcitx 5 模块和客户端库加载，以及应用与 WebKit 进程启动。自动检查使用 Xvfb；Cinnamon 等实际桌面的候选框、切换输入法和中文上屏仍需实机验证。

## 没有 FUSE

使用运行时的解包启动模式：

```sh
./tamias-0.2.0-beta.3-linux-amd64.AppImage --appimage-extract-and-run
```

这会临时解包运行组件，需要额外临时空间。

## Ubuntu 的用户命名空间限制

Ubuntu 24.04 及之后的系统可能通过 AppArmor 限制非特权用户命名空间，使 WebKit 启动时报 `bwrap: setting up uid map: Permission denied`。这种情况下需要管理员为应用配置授权；单纯添加执行权限不能解决。

确认下载来源和 Release 中的 SHA-256 校验值后，可将 AppImage 固定在下列路径，并按 Ubuntu 官方建议添加仅匹配该路径的配置。此配置允许该程序创建用户命名空间，以运行 WebKit 沙箱。

```sh
mkdir -p "$HOME/Applications"
cp tamias-0.2.0-beta.3-linux-amd64.AppImage "$HOME/Applications/tamias.AppImage"
chmod +x "$HOME/Applications/tamias.AppImage"

sudo tee /etc/apparmor.d/tamias-appimage >/dev/null <<EOF
abi <abi/4.0>,
include <tunables/global>
profile tamias-appimage "$HOME/Applications/tamias.AppImage" flags=(unconfined) {
  userns,
}
EOF
sudo apparmor_parser -r /etc/apparmor.d/tamias-appimage
"$HOME/Applications/tamias.AppImage"
```

移动 AppImage 后须同步调整配置中的路径。卸载此授权：

```sh
sudo apparmor_parser -R /etc/apparmor.d/tamias-appimage
sudo rm /etc/apparmor.d/tamias-appimage
```

依据：[Ubuntu 24.04 发行说明中的用户命名空间限制](https://discourse.ubuntu.com/t/ubuntu-24-04-lts-noble-numbat-release-notes/39890)。保留 WebKit 沙箱；无需全局关闭 AppArmor 或用户命名空间限制。
