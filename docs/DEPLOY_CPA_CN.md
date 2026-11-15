# 将自修改 Cursor 插件 0.1.4 安装到 CPA v8.0.66

本包只更新 `spx-cursor` 插件，不更换 CPA 镜像。它修复错误详情透传、有效结束帧识别和已接收帧中的 usage/cache 丢失；不承诺提升上游缓存命中率或降低延迟。默认传输仍为 `http1`。

本次 Linux 包适用于 **Linux AMD64、glibc 2.34 或更高版本**，使用 Debian Bookworm 工具链构建；它不是 manylinux/glibc 2.17 包。CPA v8.0.66 官方 Debian Bookworm 镜像符合此要求。准确构建环境、源码提交、实测结果和 SHA-256 见同目录 `BUILDINFO.json`、`SHA256SUMS`。不要用于 ARM64 或 Alpine/musl；Windows 包也不能放入 Linux Pod。

## CPA 能否导入本地插件

CPA v8.0.66 管理 API 提供 `POST /v8/management/plugins/store/<id>/install`，它从配置的商城源下载 Release；没有通用的本地 `.so` 或 ZIP 上传接口。当前商城源仍指向原作者仓库，点击商城安装/更新不会得到本包，并可能覆盖本包。

当前使用手动安装。若以后需要商城更新，应先把包发布到自己的仓库，配置与该仓库匹配的 registry，再切换来源；不要让上游仓库地址冒充自修改版本的来源。

## 1. 确认实际加载位置并备份

在 CPA 管理面板插件列表，或使用 `GET /v8/management/plugins`，确认：

- 全局 `plugins_dir`、当前 `spx-cursor` 加载路径和版本。
- Pod 的 CPU 架构为 `amd64`，插件目录位于持久卷或明确的镜像路径。
- 配置由 Rancher、ConfigMap、Secret、GitOps 或其他哪一处维护。
- 需要停机更新时，记录受影响 Workload 的原副本数，更新完成后按原值恢复。

不要只复制进容器临时层；Pod 重建会丢失。Rancher 中检查 Workload 的卷挂载，以及是否有 initContainer 每次重新安装上游插件。启用了 Home 插件同步时，也要先使同步目标与自修改包一致，或在验证实例停用该同步。

把当前插件文件、原 `plugins.configs.spx-cursor` 配置和哈希备份到**插件扫描目录之外**。CPA 会清理同 ID 未选中的旧动态库，所以不能把回滚副本并排留在 `<plugins-dir>` 或 `<plugins-dir>/linux/amd64`。

优先为验证实例使用独立插件目录或独立 PVC，先停止/缩容它，再修改插件文件。不要向仍运行的动态库原地写字节。若多个 Pod 共享同一个插件目录，旧 Pod 仍固定 `0.1.3` 时可能把新放入的 `0.1.4` 当作未选版本删除；普通滚动重启不足以避免这个问题。此时应在维护窗口停止所有消费该目录的 CPA 实例，用维护 Pod 同时更新文件与版本选择配置，再恢复副本。

## 2. 上传并安装

将本包上传到运维可访问的安全目录，解压外层交付 ZIP。在容器/辅助 Pod 的插件持久卷中放置库。标准商城 ZIP 名为：

```text
spx-cursor_0.1.4_linux_amd64.zip
```

其根目录是 `spx-cursor.so`；CPA 按版本选择时的安装文件名应为：

```text
<plugins-dir>/linux/amd64/spx-cursor-v0.1.4.so
```

交付目录同时提供已按安装名命名的库。先验证交付包的 `SHA256SUMS`，再安装。以下命令在已挂载插件 PVC 的维护 Pod 或节点上运行，路径必须替换为真实值；不包含任何认证信息：

```sh
cd /path/to/unpacked-release
sha256sum -c SHA256SUMS

PLUGIN_DIR=/actual/persistent/plugins
BACKUP_DIR=/actual/persistent/plugin-backups/spx-cursor-before-0.1.4
mkdir -p "$BACKUP_DIR" "$PLUGIN_DIR/linux/amd64"
# 在停止待更新实例后，将原库和原插件配置复制到 BACKUP_DIR。
install -m 0755 spx-cursor-v0.1.4.so "$PLUGIN_DIR/linux/amd64/.spx-cursor-v0.1.4.so.new"
mv "$PLUGIN_DIR/linux/amd64/.spx-cursor-v0.1.4.so.new" "$PLUGIN_DIR/linux/amd64/spx-cursor-v0.1.4.so"
sha256sum "$PLUGIN_DIR/linux/amd64/spx-cursor-v0.1.4.so"
```

`mv` 需要在同一文件系统内完成。最后输出必须与 `BUILDINFO.json` 中 Linux 库的 SHA-256 一致。确保文件和父目录对 CPA 容器 UID/GID 可读可执行；不需要对整个 PVC 做无差别 `chown`。`kubectl cp` 需要目标容器有 tar；否则使用已挂载该 PVC 的维护 Pod 上传文件。可参考如下命令，但不要将整个 release 目录复制到插件扫描目录：

```sh
kubectl -n <namespace> cp ./spx-cursor-v0.1.4.so <maintenance-pod>:/staging/spx-cursor-v0.1.4.so -c <container>
```

## 3. 修改版本选择配置

在配置的真实来源里**局部合并**以下字段，保留原 `store` 中其他来源信息与其余账号配置，不要把整个生产配置替换为本示例：

```yaml
plugins:
  enabled: true
  dir: /actual/container/plugins
  configs:
    spx-cursor:
      enabled: true
      http-transport: http1
      store:
        version: "0.1.4"
        release-tag: "v0.1.4"
```

这两个 `store` 字段用于本地文件版本选择；继续指向 `0.1.3` 会选错文件或导致加载失败。它们不会自动创建一个真实的上游 Release。若部署使用 Home、initContainer 或其他自动同步，先调整该流程，避免其随后从原作者仓库重新安装。

## 4. 启动、验证和回滚

若此前缩容至零，先恢复记录的副本数；单独 `rollout restart` 不会把零副本恢复为非零。随后重启验证实例并在 Rancher 查看 rollout 状态；命令行示例：

```sh
kubectl -n <namespace> scale deployment/<cpa-deployment> --replicas=<recorded-replicas>
kubectl -n <namespace> rollout restart deployment/<cpa-deployment>
kubectl -n <namespace> rollout status deployment/<cpa-deployment>
```

验证以下三项，不能只看 Pod Ready：

1. `GET /v8/management/plugins` 或插件列表显示 `spx-cursor` 已注册，Version 为 **0.1.4**，路径为目标 `spx-cursor-v0.1.4.so`。
2. 容器内文件 SHA-256 与交付库一致，启动日志没有插件加载/ABI 错误。
3. `/v1/models` 含预期 Cursor 模型，一条流式请求有正文、结束原因、`[DONE]` 和 usage。

保留当前 `http1`。`host-default` 会让 CPA 自行协商协议，但不强制 HTTP/2，也不自动建立插件连接池。本次发布验证没有证明它能改善实际连接复用或慢请求。

如果异常，停止同一实例，恢复扫描目录之外的原插件文件和**原版本选择配置**，重启后核对旧版本及旧哈希。不要只恢复旧库而继续把 `store.version` 固定为 `0.1.4`。确认新版本稳定前，保留原文件与配置备份。

## 本地验证与正式发布边界

本包已执行的检查以 `BUILDINFO.json` 为准；构建用的 Linux 二进制是新 0.1.4，不能把旧 0.1.3 改名冒用。正式公开的五平台 GitHub Release 仍需现有 CI 在各平台构建、C ABI 检查和全包校验；本交付只包含实际构建的目标平台，没有推送代码、打 tag 或创建公开 Release。
