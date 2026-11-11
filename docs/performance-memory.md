# EdgeNode 内存与 CPU 预算

## 已修复的资源放大

过去 `SystemMemoryGB()` 读取宿主机总内存、向下取整后至少返回 1 GiB；Go 软限制又由该整数计算，可能覆盖显式 `GOMEMLIMIT`。容器即使仅有 512 MiB，也会按宿主机或 1 GiB 设置预算。内存缓存还存在每个存储至少 1 GiB 的下限，多个策略会继续放大占用。

现在 `SystemMemoryBytes()` 首次调用就初始化，Linux 上取宿主机总量与当前 cgroup、可见祖先有限额的最小值。支持 cgroup v2 `memory.max` / `memory.current`、v1 `memory.limit_in_bytes` / `memory.usage_in_bytes`，按实际挂载点和挂载根解析路径。可用量还受祖先“限额减用量”约束，因此兄弟工作负载也会减少余量。`max`、v1 unlimited 标记、零值、无法解析的值不会误设为零预算。层级语义参见 [Linux cgroup v2 文档](https://docs.kernel.org/admin-guide/cgroup-v2.html) 与 [v1 内存控制器文档](https://docs.kernel.org/admin-guide/cgroup-v1/memory.html)。

预算每 10 秒刷新，cgroup 迁移和额度变化会在后续快照中生效。读取接口使用原子不可变快照，初始化后不读文件、不分配内存；原先可用内存读写的竞争也已消除。宿主机统计暂时失败时保留最近有效快照，初始完全未知时字节接口返回 0。无法读取 cgroup 信息时退回宿主机信息，可通过显式配置约束预算。

## 512 MiB 示例与边界

假设有效总量为 512 MiB，启用内容内存层的存储数为 `N >= 1`，包括独立内存策略和磁盘策略启用的内存层；纯磁盘策略不分摊：

| 项目 | 自动预算 | 含义 |
| --- | --- | --- |
| 内存内容缓存 | 每个存储约 `170.7 / N MiB` | 总内容预算约有效内存的三分之一，策略可配置更小 |
| 打开文件缓存 | 32 MiB 逻辑文件内容 | 按缓存文件 body 的尺寸计费，不表示分配了 32 MiB heap/RSS |
| Go 软内存限制 | 约 409.6 MiB | 默认有效字节数的 80%，可由运维覆盖 |

这些数值属于不同层次：Go 缓存对象也计入 Go 管理内存，不能把各行直接相加作为进程占用估算。打开文件缓存保存文件句柄及元数据，`capacitySize` 限制的是关联文件 body 的逻辑尺寸，驻留内存需另行测量。缓存元数据、队列和并发请求还会额外占用内存。旧 `SystemMemoryGB()` 为兼容调用方仍至少返回 1；TTL/WAF/统计等以 GiB 推算的其他容量仍需逐项预算化，尚未统一为精确字节。

`GOMEMLIMIT` 和 `debug.SetMemoryLimit` 是 Go 管理内存的软目标，不能保证进程 RSS 或 cgroup 总量不超过它。SQLite/图片编解码等 CGO 分配、mmap、内核网络缓冲及文件页缓存另需留量；共享 cgroup 中其他进程也会争用额度。工作集无法缩小时，Go 可能超过软限制。依据见 [Go SetMemoryLimit 文档](https://pkg.go.dev/runtime/debug#SetMemoryLimit)。

显式 `GOMEMLIMIT=320MiB`、`GOMEMLIMIT=off` 均保留；已有程序设置的限制优先。后台如果观察到其他组件改写限制，就停止自动覆盖。内容缓存仍按系统/cgroup 有效总量的三分之一分配，不会随较低的 `GOMEMLIMIT` 自动同比缩减；只降软限制可能让缓存工作集挤占运行空间、增加 GC CPU。应配合下调缓存容量、队列与并发工作集，并在实际流量下验证。这里的 512 MiB 是计算示例，未证明该容量足够运行生产 CDN/WAF。

## CPU 配置优先级

节点 `MaxCPU > 0` 时使用明确上限，最多为 `runtime.NumCPU()`。归零后，优先恢复合法正整数 `GOMAXPROCS` 环境设置；未设置时调用 `runtime.SetDefaultGOMAXPROCS()`，恢复 Go 对 CPU affinity 和 cgroup quota 的自动跟踪。以前直接设置 `GOMAXPROCS(NumCPU())` 会关闭该跟踪。该默认值通常至少为 2，并不等于容器 CPU 的绝对限额；实际节流由内核控制。详见 [Go 容器调度说明](https://go.dev/blog/container-aware-gomaxprocs) 与 [SetDefaultGOMAXPROCS 文档](https://pkg.go.dev/runtime#SetDefaultGOMAXPROCS)。

## 验证与调优

```sh
go test -race ./internal/utils/mem
go test -race -tags=community ./internal/nodes -run '^TestApplyMaxCPU'
go test ./internal/utils/mem -run '^$' -bench 'BenchmarkSystemMemoryBytes' -benchmem -count=3
```

Go 1.27.4、Linux amd64、AMD EPYC 7643 测试机的缓存字节 getter 为 2.47–2.58 ns/op、0 B/op、0 allocs/op；并行 benchmark 为 0.44–0.48 ns/op 的摊销吞吐量。此结果验证查询本身的开销，不代表生产吞吐、RSS 或 CPU 的改善百分比。额度及环境优先级测试使用内存文件样例和注入函数，不修改真实 cgroup 或测试进程的全局调度设置。

另将该包的测试二进制放入 `--network=none --read-only --cpus=1` 的独立 Docker 容器，只运行 `TestSystemMemoryGB`：`--memory=256m` 与 `--memory=512m` 分别准确返回 `268435456` 和 `536870912` 字节，兼容 GiB 接口均返回 1。这验证真实容器额度识别；未启动节点、未压测业务，也不表示 CDN/WAF 工作集能在 256/512 MiB 内运行。

测试环境可使用已有命令启动诊断节点并在相同请求集下采样：

```sh
./bin/edge-node pprof -addr 127.0.0.1:6060
go tool pprof -top 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
go tool pprof -top 'http://127.0.0.1:6060/debug/pprof/heap'
```

`pprof` 命令启动一个节点实例，应在已配置的独立测试环境使用。比较修改前后相同命中率、对象大小、TLS/压缩/WAF 设置和并发数，记录 RSS、cgroup `memory.current` / `memory.events`、GC CPU、CPU throttling、P95/P99、回源数与错误率。静态文件命中、回源大文件、小对象高并发和 WAF 重负载应分别压测，容量下降造成命中率变差时也要检查回源带宽与延迟。
