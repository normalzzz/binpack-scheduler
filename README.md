# BinPack Scheduler

基于 Kubernetes Scheduling Framework 的自定义调度器，通过 `BinPack` 评分插件，将新 Pod 优先放到资源 requests 占比较高的可用节点上，尽量集中使用节点资源。

当前依赖 Kubernetes **v1.36.5**，面向 Kubernetes **1.36** 集群。构建需要 **Go 1.26.0 或更高版本**。

## 调度逻辑

`BinPack` 实现 `ScorePlugin`，对通过 Filter 阶段的候选节点计算分数：

```text
CPU 分数 = min(100, floor((节点已有 CPU requests + 当前 Pod CPU requests) / 节点可分配 CPU × 100))
内存分数 = min(100, floor((节点已有内存 requests + 当前 Pod 内存 requests) / 节点可分配内存 × 100))
BinPack 分数 = floor((CPU 分数 × CPU 权重 + 内存分数 × 内存权重) / 权重之和)
```

可分配资源或请求总量小于等于 0 时，该资源得分为 0。CPU 和内存的内部权重目前均为 `1`，在 [main.go](main.go) 中定义。

评分使用资源 **requests**，不读取实时 CPU、内存使用率。Pod requests 通过 Kubernetes 的资源辅助函数计算，并结合代码中使用的 feature gate。其他已启用的评分插件也会影响最终节点选择，因此不能保证每次都选中 requests 占比最高的节点。

此插件参与新 Pod 的调度，已运行的 Pod 不会因为调整配置而自动迁移。

## 项目文件

| 文件 | 用途 |
| --- | --- |
| [main.go](main.go) | BinPack 评分逻辑和调度器入口 |
| [go.mod](go.mod) | Kubernetes 依赖版本及 staging 模块替换规则 |
| [Dockerfile](Dockerfile) | 构建包含插件的调度器镜像 |
| [deploy/rbac.yaml](deploy/rbac.yaml) | ServiceAccount、ClusterRole 和权限绑定 |
| [deploy/binpack-scheduler-configmap.yaml](deploy/binpack-scheduler-configmap.yaml) | 调度器配置与选主设置 |
| [deploy/binpack-scheduler-deployment.yaml](deploy/binpack-scheduler-deployment.yaml) | 调度器双副本部署 |
| [example/nginx-deployment.yaml](example/nginx-deployment.yaml) | 使用该调度器的工作负载示例 |

## 本地编译

在仓库根目录执行：

```bash
go mod download
mkdir -p bin
go build -o bin/binpack-scheduler .
./bin/binpack-scheduler --help
```

`go.mod` 将 `k8s.io/kubernetes` 固定为 `v1.36.5`，对应 staging 模块通过 `replace` 固定为 `v0.36.5`。升级 Kubernetes 依赖时，需要一起调整这些版本，并检查插件接口是否变化。

## 构建并推送镜像

先登录自己的镜像仓库，将下面的地址改为实际仓库地址。示例构建 Linux AMD64 镜像：

```bash
BINPACK_IMAGE='registry.example.com/team/binpack-scheduler:v1.36.5'

docker buildx build \
  --platform linux/amd64 \
  --tag "$BINPACK_IMAGE" \
  --push .
```

ARM64 节点使用 `--platform linux/arm64`。镜像架构需要与调度器 Pod 所在节点一致；跨架构构建需要构建器支持对应平台。参数说明见 [Docker Buildx 文档](https://docs.docker.com/reference/cli/docker/buildx/build/)。

镜像中的启动文件为 `/app/binpack-scheduler`。将 [Deployment](deploy/binpack-scheduler-deployment.yaml) 的 `image` 占位值替换为刚推送的完整镜像地址，并确保节点能够拉取该镜像。

## 部署到集群

使用有权创建相应 RBAC 资源及 `kube-system` 工作负载的身份执行以下命令。

当前部署清单包含以下设置：

- 命名空间：`kube-system`。
- Deployment 与 ServiceAccount：`binpack-scheduler-eks`。
- 副本数：`2`，通过 `kube-system/binpack-scheduler` Lease 选主。
- 调度器 Pod 仅允许落在没有 `karpenter.sh/nodepool` 标签的节点上。
- 两个调度器副本必须位于不同节点，集群需要至少两个满足条件且资源充足的可调度节点。
- 以 UID `1000` 运行，根文件系统只读；HTTPS 端口为 `10259`，探针使用 `/livez` 和 `/readyz`。

请按集群实际情况调整副本数及节点亲和性。这些亲和性设置约束的是调度器自身的 Pod；业务 Pod 的候选节点由业务 Pod 的约束决定。

当前 Deployment 使用默认滚动更新策略。配合强制反亲和性，更新时可能需要第三个符合条件的节点；如果只有两个节点，可将 `spec.strategy.rollingUpdate` 设置为 `maxSurge: 0`、`maxUnavailable: 1`，允许逐个替换副本。

替换镜像地址后，依次执行：

```bash
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/binpack-scheduler-configmap.yaml
kubectl apply -f deploy/binpack-scheduler-deployment.yaml

kubectl -n kube-system rollout status deployment/binpack-scheduler-eks
kubectl -n kube-system get pods -l app.kubernetes.io/name=binpack-scheduler-eks -o wide
kubectl -n kube-system get lease binpack-scheduler -o yaml
```

容器内通过 ServiceAccount 访问 Kubernetes API，当前配置无需额外挂载 kubeconfig。RBAC 清单还引用了集群内置的 `system:volume-scheduler` ClusterRole。

## 调度器配置

程序通过 `--config` 加载 `kubescheduler.config.k8s.io/v1` 格式的 `KubeSchedulerConfiguration`。完整配置位于 [ConfigMap](deploy/binpack-scheduler-configmap.yaml) 的 `data.binpack-scheduler-config.yaml` 中，当前 profile 为：

```yaml
profiles:
  - schedulerName: binpack-scheduler
    plugins:
      score:
        disabled:
          - name: NodeResourcesFit
          - name: NodeResourcesBalancedAllocation
          - name: PodTopologySpread
        enabled:
          - name: BinPack
            weight: 1
```

这里禁用的是三个插件的 **Score 扩展点**。资源是否足够等 Filter 检查仍然保留，拓扑分布的硬性过滤约束也仍然生效；其余默认插件继续参与调度。配置机制见 [Kubernetes 1.36 调度器配置文档](https://v1-36.docs.kubernetes.io/docs/reference/scheduling/config/)。

`weight` 表示整个 BinPack 插件在最终评分中的权重。插件内部的 `cpuWeight`、`memoryWeight` 当前为代码常量，`New()` 尚未解析 `pluginConfig.args`；修改内部权重需要修改代码并重新构建镜像。

修改 ConfigMap 后，需要重启调度器加载新配置：

```bash
kubectl apply -f deploy/binpack-scheduler-configmap.yaml
kubectl -n kube-system rollout restart deployment/binpack-scheduler-eks
kubectl -n kube-system rollout status deployment/binpack-scheduler-eks
```

## 让工作负载使用 BinPack

在 Deployment 的 Pod 模板中设置调度器名称，并为容器配置符合实际需求的 requests。下面是需要合并到现有 Deployment 中的字段片段：

```yaml
spec:
  template:
    spec:
      schedulerName: binpack-scheduler
```

独立 Pod 的对应字段是 `spec.schedulerName`，名称必须与调度器 profile 一致。

仓库中的 [Nginx 示例](example/nginx-deployment.yaml) 已设置该字段，并包含 `nginx-test` 命名空间及三个副本。运行前检查示例的镜像、资源和容器安全设置是否适合你的环境；当前 CPU limit 为 `"250"`（250 核），如需 250 毫核应改为 `"250m"`。

```bash
kubectl apply -f example/nginx-deployment.yaml
kubectl -n nginx-test get pods -o wide
kubectl -n nginx-test get events --sort-by=.metadata.creationTimestamp
```

观察 Pod 的 `NODE` 字段及调度事件来确认调度结果。容器启动与探针状态需结合应用日志单独判断。

## 日志与排查

```bash
kubectl -n kube-system logs \
  -l app.kubernetes.io/name=binpack-scheduler-eks \
  --all-containers=true --prefix=true --tail=100

kubectl -n kube-system describe pods \
  -l app.kubernetes.io/name=binpack-scheduler-eks
```

Deployment 当前设置 `--v=5`，满足 `BinPack score` 日志所需的 `--v=4`。该日志包含 Pod、候选节点、加入 Pod 后的 CPU/内存 requests 及评分；实际处理调度的 leader 会输出这些评分日志。

| 现象 | 检查方向 |
| --- | --- |
| `ImagePullBackOff` | Deployment 是否替换为实际镜像地址，镜像是否已推送，以及节点的拉取权限 |
| 调度器 Pod 长时间 Pending | 节点标签、可用资源、污点，以及双副本的强制反亲和性 |
| 提示找不到 `BinPack` 插件 | 镜像是否通过本项目 Dockerfile 构建，启动文件是否为 `/app/binpack-scheduler` |
| 业务 Pod 长时间 Pending | `schedulerName` 是否匹配，调度器是否有 leader，以及 Pod 的资源、亲和性、存储约束和调度事件 |
| API 请求返回 `Forbidden` | RBAC 是否已应用，ServiceAccount 是否匹配，并根据日志检查具体资源和操作权限 |
| 配置修改未生效 | ConfigMap 内容及挂载路径是否正确，调度器是否已重启 |

## 许可证

[MIT](LICENSE)。
