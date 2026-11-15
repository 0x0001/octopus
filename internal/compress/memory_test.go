package compress

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// flushWriter 统一各算法编码器的测量接口, Reset 供速度测量在轮间复用实例,
// 避免构造开销(zstd 构造达毫秒级)污染压缩计时。gzip/deflate(zlib)/brotli/zstd
// 四种算法的编码器原生满足该接口。
type flushWriter interface {
	io.WriteCloser

	Flush() error
	Reset(w io.Writer)
}

// residentPerStream 构造 count 个编码器, 各写入约 1.5 倍窗口的数据(确保窗口缓冲
// 与级别相关的查找结构全部按需分配), 不做 GC —— 活跃流的工作态就是线上常驻形态;
// 用 HeapAlloc 差值除以数量得到单流持有量, 同时返回 goroutine 增量。
func residentPerStream(t *testing.T, count int, warmup []byte, factory func(io.Writer) flushWriter) (perStreamBytes float64, goroutines int) {
	t.Helper()
	runtime.GC()
	var before runtime.MemStats
	gBefore := runtime.NumGoroutine()
	runtime.ReadMemStats(&before)

	writers := make([]flushWriter, count)
	for i := range writers {
		w := factory(io.Discard)
		if _, err := w.Write(warmup); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		writers[i] = w
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	total := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if total < 0 {
		total = 0
	}
	return float64(total) / float64(count), runtime.NumGoroutine() - gBefore
}

// TestCompressionMemoryReport 全算法工作态常驻内存矩阵:
// 级别/质量与窗口各贡献多少常驻, 与压缩比/速度同表对比, 锁定各算法参数的安全边界。
// 采样数按窗口大小缩放控制内存预算; zstd 关键档位纳入同一方法保证横向同源可比。
func TestCompressionMemoryReport(t *testing.T) {
	events := sseCorpus()
	total := corpusBytes(events)

	type row struct {
		name       string
		residentKB float64
		goroutines int
		compressed int
		speedMBps  float64
	}
	var rows []row

	run := func(name string, count int, warmup []byte, factory func(io.Writer) flushWriter) {
		// 常驻内存(构造 + 写满窗口的工作态)。
		perStream, goroutines := residentPerStream(t, count, warmup, factory)

		// 压缩比: 独立 sink, 逐事件 flush 与线上节奏一致。
		ratioSink := &bytes.Buffer{}
		enc := factory(ratioSink)
		for _, event := range events {
			if _, err := enc.Write(event); err != nil {
				t.Fatal(err)
			}
			if err := enc.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		if err := enc.Close(); err != nil {
			t.Fatal(err)
		}
		compressed := ratioSink.Len()

		// 速度: 构造一次, 轮间 Reset 换新 sink, 三轮取最好。
		best := int64(1 << 62)
		for round := 0; round < 3; round++ {
			sink := &bytes.Buffer{}
			enc.Reset(sink)
			start := time.Now()
			for _, event := range events {
				if _, err := enc.Write(event); err != nil {
					t.Fatal(err)
				}
				if err := enc.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			if d := time.Since(start); int64(d) < best {
				best = int64(d)
			}
		}
		speed := float64(total) / float64(best) * 1e9 / 1e6

		rows = append(rows, row{name, perStream / 1024, goroutines, compressed, speed})
	}

	const (
		win32K = 32 << 10
		win64K = 64 << 10
		win4M  = 4 << 20
		win16M = 16 << 20
	)
	warmupFor := func(window int) []byte {
		w := make([]byte, window+window/2)
		for i := range w {
			w[i] = byte('a' + i%26)
		}
		return w
	}

	// gzip: 窗口固定 32K, 级别只影响查找结构。
	run("gzip BestSpeed      ", 200, warmupFor(win32K), func(w io.Writer) flushWriter {
		return mustGzip(t, w, gzip.BestSpeed)
	})
	run("gzip Default(当前)   ", 200, warmupFor(win32K), func(w io.Writer) flushWriter {
		return mustGzip(t, w, gzip.DefaultCompression)
	})
	run("gzip BestCompression", 200, warmupFor(win32K), func(w io.Writer) flushWriter {
		return mustGzip(t, w, gzip.BestCompression)
	})

	// deflate: 与 gzip 同内核(zlib 帧), 内存特征预期一致。
	run("deflate BestSpeed      ", 200, warmupFor(win32K), func(w io.Writer) flushWriter {
		return mustDeflate(t, w, zlib.BestSpeed)
	})
	run("deflate Default(当前)   ", 200, warmupFor(win32K), func(w io.Writer) flushWriter {
		return mustDeflate(t, w, zlib.DefaultCompression)
	})
	run("deflate BestCompression", 200, warmupFor(win32K), func(w io.Writer) flushWriter {
		return mustDeflate(t, w, zlib.BestCompression)
	})

	// brotli: 质量 × 窗口双维度。
	run("br q4  lgwin16     ", 200, warmupFor(win64K), func(w io.Writer) flushWriter {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 4, LGWin: 16})
	})
	run("br q5  lgwin16(当前)", 200, warmupFor(win64K), func(w io.Writer) flushWriter {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 5, LGWin: 16})
	})
	run("br q9  lgwin16     ", 200, warmupFor(win64K), func(w io.Writer) flushWriter {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 9, LGWin: 16})
	})
	run("br q11 lgwin16     ", 200, warmupFor(win64K), func(w io.Writer) flushWriter {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 11, LGWin: 16})
	})
	run("br q5  lgwin22(4M) ", 20, warmupFor(win4M), func(w io.Writer) flushWriter {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 5, LGWin: 22})
	})
	run("br q5  lgwin24(16M)", 10, warmupFor(win16M), func(w io.Writer) flushWriter {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 5, LGWin: 24})
	})

	// zstd: 关键档位纳入同源对比。
	run("zstd lvl默认 w64K(当前)", 200, warmupFor(win64K), func(w io.Writer) flushWriter {
		return mustZstd(t, w, zstd.WithWindowSize(win64K), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
	})
	run("zstd lvl最大 w64K      ", 200, warmupFor(win64K), func(w io.Writer) flushWriter {
		return mustZstd(t, w, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(win64K), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
	})
	run("zstd lvl最大 w8M 并发4 ", 20, warmupFor(8<<20), func(w io.Writer) flushWriter {
		return mustZstd(t, w, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithWindowSize(8<<20), zstd.WithEncoderConcurrency(4), zstd.WithEncoderCRC(false))
	})

	t.Logf("SSE 语料: %d 字节; 常驻内存为工作态(已写满各自窗口)按采样数取均值", total)
	for _, r := range rows {
		ratio := float64(total) / float64(r.compressed)
		t.Logf("%-24s 常驻 %8.0f KB/流  goroutine +%-2d  压缩后 %7d 字节 (%5.2fx)  速度 %6.1f MB/s",
			r.name, r.residentKB, r.goroutines, r.compressed, ratio, r.speedMBps)
	}
}

func mustGzip(t *testing.T, w io.Writer, level int) flushWriter {
	t.Helper()
	writer, err := gzip.NewWriterLevel(w, level)
	if err != nil {
		t.Fatal(err)
	}
	return writer
}

func mustDeflate(t *testing.T, w io.Writer, level int) flushWriter {
	t.Helper()
	writer, err := zlib.NewWriterLevel(w, level)
	if err != nil {
		t.Fatal(err)
	}
	return writer
}

func mustZstd(t *testing.T, w io.Writer, opts ...zstd.EOption) flushWriter {
	t.Helper()
	writer, err := zstd.NewWriter(w, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return writer
}
