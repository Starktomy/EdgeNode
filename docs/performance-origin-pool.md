# 回源连接池性能

原连接池在 4096 个客户端达到上限后，每次新建客户端都持有全局写锁扫描全部条目。现在只检查最多 16 个候选，淘汰其中访问时间最早者；关闭被淘汰客户端的空闲连接放在锁外。注册表仍严格限制为 4096 个，定时空闲清理保留。

这是近似最近访问淘汰：它可能提前淘汰采样内的活跃条目，不能保证选到全局最旧者。高源站数量场景应同时观察连接复用率、TLS 握手数及 P99，以确认缩短锁占用的收益。

## 默认连接预算

普通回源的自动空闲连接数由 `max(NumCPU, 8) * 16` 改为 `min(max(GOMAXPROCS, 1), 8) * 16`。内部节点的倍率计算后，自动值也限制为 128；PROXY protocol 保留 3 个空闲连接。比如 `GOMAXPROCS=2` 时普通回源默认保留上限为 32。

源站显式 `MaxIdleConns`（源站配置字段 `idleConns`）继续按已有节点层级倍率处理。活动连接上限 `MaxConns`、连接/读取超时及读缓冲保持原有逻辑；较低的空闲上限可能增加突发流量的重新建连，应按实测复用率调整。上限在创建 Transport 时确定，后续 CPU 变化不会重建已有连接池。

## 本地基准

Go 1.27.4、Linux/amd64、AMD EPYC 7643，`GOMAXPROCS=2`，每组运行 5 次、每次 300ms。基准预填满注册表，并循环访问两倍于容量的不同 Host，触发命中和淘汰；不发网络请求。优化前基于 `3d555b963a` 的实现，表中为中位数。

| 客户端容量 | ns/op：前 → 后 | B/op：前 → 后 | allocs/op：前 → 后 |
| --- | ---: | ---: | ---: |
| 64 | 1519 → 1394 | 946 → 941 | 7 → 7 |
| 4096 | 48227 → 1628 | 1080 → 938 | 8 → 7 |

4096 场景局部耗时约降低 96.6%。这是注册表持续变化的微基准，且采样淘汰会影响命中分布；不能将其当作真实回源延迟或全站 QPS 的提升比例。原始结果：[优化前](performance-origin-pool-before.txt)、[优化后](performance-origin-pool-after.txt)。

```sh
GOTOOLCHAIN=local GOMAXPROCS=2 go test -tags=community \
  ./internal/nodes -run '^$' -bench '^BenchmarkOriginPoolLookup$' \
  -benchmem -benchtime=300ms -count=5
```

## 回归验证

测试覆盖并发源站切换期间的容量上限、采样中的较早条目替换、锁外空闲连接关闭、默认与显式空闲上限。既有源站 TLS 校验、PROXY 元数据快照及重试安全测试继续执行。请求、源站身份、PROXY 源/目标地址及协议版本参与连接池隔离的逻辑保持一致。

```sh
GOTOOLCHAIN=local GOMAXPROCS=2 go test -tags=community -race \
  ./internal/nodes -run '^TestSecurity(Origin|Proxy)' -count=1
```
