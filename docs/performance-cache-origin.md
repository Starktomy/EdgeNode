# HTTP 缓存元数据性能分析

本次优化缓存写入时的 `Vary` 元数据生成，以及命中缓存时的请求头匹配。带 `Origin` 和语言差异的写入基准耗时中位数降低约 41%；模拟压缩增加 `Vary` 字段的场景降低约 43%。这些结果衡量局部处理成本，不能推算整个 CDN 的 QPS。

## 发现与实现

- `HTTPWriter.PrepareCache` 原来在验证、计算头长度和写入缓存时分别执行 JSON/Base64 编码。现在每个响应保存编码后的完整内部头行，并复用长度；压缩或其他转换修改 `Vary` 时重新生成。
- 仅记录原始客户端请求的不可变快照。保存自有的 `Vary` 行切片，能识别响应头切片被原地修改；没有跨请求共享或全局元数据缓存。
- `MatchesVary` 原来复制当前请求头值并拼接字符串。现在优先逐行去除首尾空白后比较；字段行边界不同才使用原来的合并比较路径。

每个 `HTTPWriter` 增加一个切片、一个字符串和两个布尔字段；使用缓存时还会保存本次响应的元数据。减少的是重复编码和临时分配，不能据此宣称所有请求的常驻内存都会降低。

## 基准方法

测量日期：2026-11-11。环境：Go 1.27.4、Linux/amd64、AMD EPYC 7643、`GOMAXPROCS=2`。改动前以 `3d555b963a` 的生产代码运行相同基准，改动后重复运行；每组 5 次，表中为中位数。共享开发主机上的微小耗时差异可能来自噪声。

在 EdgeNode 模块根目录、使用上述 Go 工具链运行：

```sh
GOTOOLCHAIN=local GOMAXPROCS=2 go test -p 2 \
  ./internal/httpcache ./internal/nodes -run '^$' \
  -bench '^(BenchmarkMatchesVary|BenchmarkCacheResponseMetadata)$' \
  -benchmem -benchtime=300ms -count=5
```

原始结果：[优化前](performance-cache-origin-before.txt)、[优化后](performance-cache-origin-after.txt)。

| 基准场景 | ns/op：前 → 后 | B/op：前 → 后 | allocs/op：前 → 后 |
| --- | ---: | ---: | ---: |
| 写入，无 Vary | 4047 → 3902 | 2192 → 2192 | 25 → 24 |
| 写入，Origin + 语言 | 12423 → 7339 | 5552 → 3472 | 73 → 41 |
| 写入，增加压缩 Vary | 20277 → 11612 | 8480 → 4920 | 115 → 64 |
| 匹配，无 Vary | 45.96 → 46.63 | 0 → 0 | 0 → 0 |
| 匹配，Origin | 1480 → 1331 | 560 → 544 | 10 → 9 |
| 匹配，Origin + 语言 | 2114 → 1985 | 656 → 624 | 12 → 10 |
| 匹配，多字段行 | 2441 → 2135 | 736 → 656 | 15 → 11 |

写入基准为每次响应创建新的 `HTTPWriter`，经过真实内存缓存的准备、头序列化和丢弃流程；不会在不同请求之间复用已编码结果。压缩场景只模拟增加 `Accept-Encoding` 后的元数据处理，不执行压缩算法。以上均不包含网络、TLS、正文传输或真实源站延迟。

## 兼容性与安全验证

缓存内部头名、JSON/Base64 格式、现有条目兼容性保持一致。回归覆盖跨 Origin 的响应头与正文隔离、压缩新增字段、原地修改 `Vary`、`Vary:*`、删除 `Vary`、原始请求快照、并发响应隔离，以及计算长度与实际头行一致。

匹配测试保留字段缺失与存在但为空的区别，并验证代理合并字段行、逗号边界和首尾空白。实际缓存的冷填充等待者、零缓存寿命和已存储表示测试也已通过以下 race 回归：

```sh
GOTOOLCHAIN=local GOMAXPROCS=2 go test -p 2 -race \
  ./internal/httpcache ./internal/nodes \
  -run '^(TestSecurity(Cache|ColdCache|StoredCache|Vary)|TestShared|TestSensitive|TestVary|TestCompression|TestConditional|TestFills)' \
  -count=1 -v
```

## 后续测量

缓存命中仍需解析 Base64/JSON；下一步应使用真实命中率和 `Vary` 分布采集 CPU、分配及锁竞争 profile，再判断是否需要解析结果缓存，并限制其容量和生命周期。回源连接池属于独立路径，应分别测量连接复用、容量耗尽、源站数量及并发创建；本报告的数据不包含连接池收益。
