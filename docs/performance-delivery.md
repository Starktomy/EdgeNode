# 流量统计与日志投递性能

## 改动与正确性边界

流量统计的 server/time、domain/time 键改用结构体，避免每个请求格式化时间戳、服务器 ID 和域名字符串。域名首次进入采样表时仍复制字符串，避免小 Host 引用整个请求缓冲区。每服务最多 256 个域名桶、上报前 20 个域名的策略保持不变；服务总流量独立完整累计。

spool 按每页 128 个名字扫描，扫描过程中只保留最早的 8 个候选，按修改时间和文件名排序。没有常驻目录索引，因此外部恢复、删除文件仍影响容量和下次投递。`Append` 与候选选择仍是 O(N) 元数据扫描。

统计和访问日志使用 `WalkPending`：每次读取一个批次，回调返回后释放其引用，再读取下一批；每轮仍最多 8 批。网络等待期间不持有 spool 锁，生产者可以继续落盘。兼容的 `Pending` 接口将返回批次的累计 payload 限制为一个 `maxBatch` 预算，可能少于 8 批。

写入仍执行文件及目录 fsync；ACK、隔离仍确认目录持久化。发布后 fsync 报错仍保留原 ID，重试不生成新身份。无效文件会隔离，隔离预算耗尽则报错并保留原文件。读取前重新验证文件类型、身份和大小，并限制实际读取长度；Unix 使用 `O_NOFOLLOW | O_NONBLOCK` 避免符号链接泄漏和 FIFO 阻塞。日志 RPC 的截止时间、及时取消和成功后 ACK 均有回归测试。

## 恢复解码预算

文件大小不能直接限制解码后的内存：8192 条空消息只需 16 KiB，却会创建数千个对象。两个 sender 在 protobuf 解码前执行零分配的 schema 校验：顶层日志/统计合计最多 2000 条，已知 schema 的 repeated、map、packed 子元素全批最多 131072 个，消息和 group 嵌套深度最多 32。正常新统计仍按 1000 条落盘；统一 2000 上限兼容 API 的日志事务上限。

校验会检查 Header → Strings、Errors、Tags、FirewallActions 和 Node 子消息；未知 bytes 保持不透明，未知字段继续兼容，只对 group 使用固定栈检查。实际解码还设置 `UnmarshalOptions.RecursionLimit=32`。旧文件超过这些预算会保留原批次 ID 和完整内容进入 quarantine，并记录明确原因；隔离空间不足则留在原位置报错。恢复需要人工审核，不会通过重新生成身份自动拆批。记录数和子元素上限也不等于整个进程或 protobuf 的严格 heap 上限。

生产端使用同一元素预算：`Push` 通过切片/map 长度及 Header 值计数检查单条日志，单条超限在入队前拒绝并增加 `Dropped`。正常日志不需要 Marshal；包含 Node 或 unknown 数据时使用 schema 对象遍历。持久化按记录数、字节数和元素数三重边界提前 carry 分批，并在发布前预检编码结果；spool 满时已验证的 pending payload 重试不重复预检。回归中的 2000 条、每条 33 个单值 Header 共 134000 元素，现拆成两批完整发送，`Dropped=Quarantined=0`，RAM 计费归零。已发布的旧文件始终沿用原 ID，不按新身份重拆。

`BenchmarkValidateBatchRecords` 测得预检 2000 条空记录约 72µs、8192 条恶意空记录约 71µs（到第 2001 条即拒绝），均为 0 B/op、0 allocs/op。2000 条日志、每条 20 个 Header 的约 1.28 MB payload 预检约 5.36ms，也为 0 分配；这是新增的恢复路径 CPU 成本，下面 spool 表只测读取，不包含该扫描或 protobuf 解码。可用 `go test ./internal/delivery -run '^$' -bench '^BenchmarkValidateBatchRecords' -benchmem -count=3` 复测。

`Push` 的新增安全计数也有 CPU 成本。以下为单 CPU、500ms、3 次中位数；包括现有 `proto.Size`、入队和基准中的出队，分配数没有增加。含 Header 的既有分配来自原尺寸计算路径，尚未在此次改动中消除。

| 单条日志 Header 数 | ns/op 前 → 后 | B/op 前 → 后 | allocs/op 前 → 后 |
| --- | ---: | ---: | ---: |
| 0 | 261.1 → 300.9 | 0 → 0 | 0 → 0 |
| 20 | 2867 → 3444 | 320 → 320 | 20 → 20 |
| 40 | 5373 → 6006 | 640 → 640 | 40 → 40 |

复测：`go test ./internal/nodes -run '^$' -bench '^BenchmarkAccessLogPushBudget$' -benchmem -benchtime=500ms -count=3 -cpu=1`。基线同样用 `3d555b9` 的原 Push 实现加新基准 overlay。

## 测量方法

基线为 `3d555b9`。环境：Go 1.27.4、Linux/amd64、AMD EPYC 7643，CGO 开启。统计每项测量 5 次、spool 每项 3 次，表中取中位数。固定域名输入、预热在计时外完成；扫描测试使用实际临时目录，Append 的文件及目录同步没有被跳过。共享机器上的耗时有波动，分配计数更稳定。

```sh
go test ./internal/stats -run '^$' \
  -bench '^BenchmarkTrafficStatManager(FixedDomain|Parallel)$' \
  -benchmem -benchtime=750ms -count=5 -cpu=1,4
go test ./internal/delivery -run '^$' \
  -bench 'BenchmarkSpool(Pending|Append)Backlog|BenchmarkSpoolDeliveryFailure' \
  -benchmem -benchtime=500ms -count=3
```

基线通过临时 Go overlay 加入同一份基准代码，没有修改旧生产代码。首次发送失败测试的旧路径调用 `Pending` 读取 8 批后模拟失败，新路径通过 `WalkPending` 在首次回调返回相同错误。该测试只覆盖磁盘读取和内存分配，不包含 protobuf 解码或真实 RPC。原始输出见 [before](performance-delivery-before.txt) 和 [after](performance-delivery-after.txt)。

## 结果

统计 `Add` 的基准包含现有带宽统计调用；剩余的 2 次分配主要来自该调用，整个请求链路仍有其他开销。

| 场景 | ns/op 前 → 后 | B/op 前 → 后 | allocs/op 前 → 后 |
| --- | ---: | ---: | ---: |
| 固定短域名，1 CPU | 306.2 → 132.5 | 104 → 16 | 8 → 2 |
| 固定长域名，1 CPU | 344.7 → 138.4 | 168 → 16 | 8 → 2 |
| 并发固定域名，4 CPU | 416.8 → 265.0 | 104 → 16 | 8 → 2 |

首批发送失败时，未访问的磁盘批次不再提前载入，每次重试的读取分配降低约 87.5%。

| 8 个待发送批次，单批大小 | ms/op 前 → 后 | B/op 前 → 后 |
| --- | ---: | ---: |
| 1 MiB | 2.255 → 0.422 | 8,472,074 → 1,064,674 |
| 4 MiB | 10.006 → 1.235 | 33,636,630 → 4,210,817 |
| 16 MiB | 31.314 → 4.040 | 134,298,537 → 16,793,377 |

满目录、每批只有几个字节时，收益主要是扫描分配减少。`Pending(4096)` 从 2,769,431 降到 1,985,643 B/op（约 28%），`Append(4095)` 从 2,408,310 降到 1,980,947 B/op（约 18%）。本轮相应耗时为 10.68 → 11.01 ms、10.03 → 11.39 ms，尚未证明目录扫描的 CPU/延迟改善。

## 内存含义与验证

旧发送路径可能同时持有 8 份编码 payload；新路径在回调不保留数据的前提下只保留当前一份。正常新日志批次最多 4 MiB，新统计批次最多 1 MiB；历史统计文件仍允许 16 MiB。日志 spool 的兼容读取上限仍为 8 MiB，避免把已发布的 4–8 MiB 历史文件当作损坏数据。这里描述的是可达编码 payload，另有 protobuf 对象、队列、GC 回收时机和页缓存开销，不能当作整机 RSS 降幅。

```sh
go test -race ./internal/delivery ./internal/stats ./internal/nodes \
  -run '^Test(DurableSpool|AppendPreserves|AckConfirms|FullSpool|CorruptSpool|QuarantineBudget|Spool|WalkPending|PendingAggregate|ValidateBatch|SecurityTraffic|TrafficUploadPreserves|AccessLog|PermanentAccessLog)' \
  -count=1
```

该检查已通过，涵盖跨目录页排序、外部恢复/删除、回调期间并发 Append/ACK、懒读取、失败重试原身份、文件变更隔离、隔离满额保留、流量部分落盘恢复与总量、日志停机落盘及 RPC 上下文取消。另覆盖解码预算边界、嵌套 map/string/packed 放大、未知字段兼容、畸形/group 深度、超限文件原 ID 隔离以及隔离满额时保留原文件。整体吞吐和 CPU 占用仍需在实际请求分布、缓存命中率和日志配置下用负载测试及 profile 验证。

有长度上限的 `FuzzValidateBatchRecords` 已用两个 worker 短跑 5 秒，完成 21924 次执行并通过。种子覆盖正常日志/统计、超额空消息、深 group、未知字段和畸形 wire；对预检及标准解码均成功的数据，还校验规范重编码后仍符合预算。运行：`go test ./internal/delivery -run '^$' -fuzz '^FuzzValidateBatchRecords$' -fuzztime=5s -parallel=2`。
