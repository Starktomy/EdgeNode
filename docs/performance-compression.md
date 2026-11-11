# 压缩池的内存与并发修复

## 已处理的问题

原来的 writer/reader `Finish` 在对象进入池后才写 `isFinished`。另一个请求可能已经取出并重置对象，随后旧请求又覆盖其状态，造成数据竞争和新请求跳过关闭、缺失压缩流尾部的风险。现在在归还前完成状态更新和引用清理，发布后不再访问对象。全局 `isBusy` 也改为原子值，保留原有 100 ms 退避策略。

压缩器关闭后可能仍保存父输出链路；当链路包含压缩缓存 tee 的错误回调时，会进一步引用 `HTTPWriter`、请求及其缓冲区。容量为 1、父对象带 1 MiB 数据的弱引用实验中，修改前 gzip/zstd 在关闭并 GC 后仍保留父对象；当前 Go 1.27 的 deflate 和 Brotli 1.3.0 已自行断开其输出引用。修改后四种编码均能释放该父对象。

四种 writer 统一使用稳定输出转接器，关闭后只清理父引用，不在归还时完整 `Reset`。下次取用仍只重置一次。gzip/deflate/brotli reader 同样通过稳定输入转接器释放原始 body 引用。只有父输入提供 `io.ByteReader` 时才选用相同能力的转接器，保留 deflate 成员后未消费字节；普通 HTTP body 继续使用原有缓冲读取。关闭或清理转接器不会额外关闭父 HTTP body。

ZSTD decoder 的 `Close` 会销毁资源，之后不能 `Reset`，原池实际上会在下次取用时重建。现在仍真正关闭资源并释放指针，直接丢弃已关闭对象。未来若改用 `Reset(nil)` 复用，还需补齐淘汰时真正关闭和内存预算；参见 [ZSTD Decoder 文档](https://pkg.go.dev/github.com/klauspost/compress/zstd#Decoder.Close)。deflate reader 改用标准库保证的 `flate.Resetter`，复用已有解码缓冲，避免每次重建约 40 KiB 状态；依据见 [flate.NewReader 文档](https://pkg.go.dev/compress/flate#NewReader)。

`Close` 归还对象后，调用方即失去所有权，应在下次借用时重新取得对象，不能保留旧引用用于写入、读取或再次关闭。

## 成本与验证

基线为 `3d555b9`；Go 1.27.4、Linux amd64、AMD EPYC 7643，CGO 开启。使用同一份临时 overlay 基准，容量为 1，30 KiB 重复文本，输入生成和预热在计时外完成，三次结果取中位数。writer 每次执行取用、写入、关闭，包括重置与同步检查：

| 编码 | ns/op 前 → 后 | B/op 前 → 后 | allocs/op 前 → 后 |
| --- | ---: | ---: | ---: |
| gzip | 11,596 → 11,756 | 0 → 0 | 0 → 0 |
| deflate | 10,463 → 10,024 | 0 → 0 | 0 → 0 |
| Brotli | 57,727 → 59,207 | 0 → 0 | 0 → 0 |
| ZSTD | 17,836 → 16,843 | 308 → 308 | 8 → 8 |

转接器没有增加稳定复用时的分配；共享机器上的时间存在波动，未证明所有编码的 CPU 吞吐提升。deflate reader 的同类解码测试由每次约 40,551 B、4 次分配降为 0 B、0 次分配，耗时约 18.2 µs 降至 7.6 µs。gzip reader 仍为 0 次分配，Brotli 为 6 次，ZSTD 为 41 次；后者仍会重新创建 decoder，本轮没有将其改造成长期缓存。

原始结果：[writer 优化前](performance-compression-before.txt)、[writer 优化后](performance-compression-after.txt)、[deflate reader 优化前](performance-compression-deflate-before.txt)、[reader 优化后](performance-compression-reader-after.txt)。

```sh
go test -race -tags=community ./internal/compressions -run '^TestSecurity' -count=3
go test ./internal/compressions -run '^$' \
  -bench '^BenchmarkCompression(Reader)?PoolReuse$' -benchmem -benchtime=500ms -count=3
```

测试覆盖四种编码的真实往返、空流、Flush、交错大小、并发复用、错误输出、重复关闭、父 body 不被额外关闭，以及 1 MiB 父对象的回收。还覆盖 gzip 拼接成员、deflate 后续帧边界，以及 `ByteReader`/普通输入在多次借用间切换。原有 handoff 和 busy 竞争已在基线小样中由 race detector 复现，修改后的安全测试连续三轮通过。

## 后续预算问题

本轮保留自动级别和容量策略。`CalculatePoolSize` 仍按至少 1 GiB 的兼容接口计算，每个池至少 32、最多 2048 个闲置对象；空池取用时会新建对象，因此它限制闲置数量，不限制同时压缩的数量。`GenerateCompressLevel` 仍在初始化时记录 `runtime.NumCPU()`，未跟踪动态 `GOMAXPROCS`。共享 writer factory 还会使用自动级别，未按传入的 level 划分池。

受控实验只预热并保留每种编码 4 个 writer：每个 gzip 约 810 KiB、deflate 约 795 KiB、Brotli 约 306 KiB、ZSTD 约 515 KiB，具体随级别和输入变化。按此小样线性估算，四种 writer 各留 32 个就约 76 MiB，尚未包括 reader、请求和内容缓存；这不是大池实测，也不是每个对象的最大值。后续应将闲置池预算改为精确字节与有效 CPU 的共同上限，并评估并发压缩许可、ZSTD 单流并行度、压缩率与尾延迟之间的取舍。未进行 OOM 测试或生产吞吐测量。
