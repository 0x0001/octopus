package compress

import (
	"compress/gzip"
	"compress/zlib"
)

// 编码参数均为小内存档: SSE 单帧很小, 大窗口不提升压缩比, 而长驻连接数量众多,
// 单流省下的内存直接决定常驻占用。基准已验证(真实 SSE 语料):
// brotli LGWin 16 与 22 压缩比逐字节一致但省 8.8MB/流分配;
// zstd 64K 与 1M 压缩比一致且省 1.1MB/流; CRC 开关对吞吐无感。各常量维持现状。
const (
	// gzip/deflate: 窗口固定 32KiB 结构性小内存(全级别 0.8-1.1MB/流), Default 为甜点:
	// 压缩比 17.4x vs BestSpeed 15.6x, BestCompression 虽 19.8x 但速度 96→22MB/s。
	writerGzipLevel    = gzip.DefaultCompression
	writerDeflateLevel = zlib.DefaultCompression

	// brotli: 质量 4 以下是快速模式分界, 5 是 Web 压缩常用档;
	// LGWin 为滑动窗口以 2 为底的对数, 16 即 64KiB(合法范围 10-24, 0 表示随质量自动)。
	// 基准(SSE 语料): LGWin 16/22/24 压缩比逐字节一致, 但常驻 1.7/23.5/68.5MB/流
	// —— 窗口绝不可调大。质量维度: q9 压缩比 18.6x 最高且速度与 q5 持平, 但 br 仅作
	// 兜底第四位几乎不会被选中, q5 保持压缩比与常驻平衡; q11 在流式场景速度崩塌
	//(0.2MB/s)且压缩比反而低于 q9, 禁用。
	writerBrotliQuality = 5
	writerBrotliLGWin   = 16

	// zstd 编码窗口 64KiB, 级别保持库默认(SpeedDefault), 单协程, 关 CRC。
	// 基准实测工作态常驻(写满窗口后): 默认级别 2.7MB/流, SpeedBestCompression
	// 高达 36MB/流(级别 match finder 表主导, 与窗口几乎无关), 换来压缩比仅
	// 14.54x→16.15x 且速度减半 —— 长驻连接下绝不可调高级别。
	writerZstdWindowSize = 64 << 10
)
