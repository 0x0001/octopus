package compress

import (
	"bytes"
	"io"
	"math/rand"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// sseCorpus 构造贴近真实 LLM 流式输出的 SSE 事件序列:
// OpenAI chat.completion.chunk 与 Anthropic content_block_delta 两种风格混合,
// 中英文本池随机拼接(固定 seed 可复现), 单事件 80-300 字节, 模拟一次长回复。
func sseCorpus() [][]byte {
	rng := rand.New(rand.NewSource(42))
	zh := []string{
		"这个问题的答案取决于具体的业务场景和约束条件。",
		"首先我们需要明确目标, 然后再拆解实现路径, 最后逐项验证假设。",
		"从性能角度来看, 减少不必要的内存分配是提升吞吐的关键手段。",
		"综上所述, 建议在生产环境中开启压缩并配合合理的超时策略。",
		"流式响应的实时性由逐事件 flush 保证, 缓冲滞留会直接放大延迟。",
	}
	en := []string{
		"The compression ratio depends heavily on the repetition structure of the payload.",
		"For streaming responses, per-event flush latency matters more than raw throughput.",
		"Choosing a small window keeps per-connection memory low at slight ratio cost.",
		"Benchmark with real payloads before locking in algorithm preference order.",
		"Keep-alive connections are long-lived, so per-stream footprint compounds quickly.",
	}
	ids := []string{"chatcmpl-9xKq8Zf2LmNpQrStUvWx", "chatcmpl-3aBcDeFgHiJkLmNoPqRs"}

	var events [][]byte
	for i := 0; i < 500; i++ {
		pool := zh
		if rng.Intn(2) == 0 {
			pool = en
		}
		text := pool[rng.Intn(len(pool))]
		if rng.Intn(3) == 0 { // 三分之一事件拼两段, 模拟长短不一的增量。
			text += pool[rng.Intn(len(pool))]
		}
		id := ids[rng.Intn(len(ids))]
		switch rng.Intn(3) {
		case 0, 1: // OpenAI chunk 风格。
			events = append(events, []byte(
				`data: {"id":"`+id+`","object":"chat.completion.chunk","created":1735689600,"model":"gpt-4o-2024-11-20","choices":[{"index":0,"delta":{"content":"`+text+`"},"finish_reason":null}]}`+"\n\n"))
		default: // Anthropic delta 风格。
			events = append(events, []byte(
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\""+text+"\"}}\n\n"))
		}
	}
	return events
}

func corpusBytes(events [][]byte) int64 {
	total := int64(0)
	for _, e := range events {
		total += int64(len(e))
	}
	return total
}

// BenchmarkStreamWriteFlush 真实 SSE 写入节奏: 逐事件 Write + Flush, 全路径开销
// (含压缩器缓冲推进), B/op 与 allocs/op 即单流内存占用, ns/event 为单事件延迟。
func BenchmarkStreamWriteFlush(b *testing.B) {
	events := sseCorpus()
	total := corpusBytes(events)

	for _, encoding := range []string{Identity, Br, Zstd, Gzip, Deflate} {
		b.Run(encoding, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(total)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var sink bytes.Buffer
				writer, err := NewWriter(&sink, encoding)
				if err != nil {
					b.Fatal(err)
				}
				for _, event := range events {
					if _, err := writer.Write(event); err != nil {
						b.Fatal(err)
					}
					if err := writer.Flush(); err != nil {
						b.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(events)), "ns/event")
		})
	}
}

// BenchmarkStreamWriteFlushVariants 当前小内存参数的对照档位:
// 验证窗口/质量参数在真实 SSE 语料上的压缩比与速度权衡, 供调整 writer.go 常量。
func BenchmarkStreamWriteFlushVariants(b *testing.B) {
	events := sseCorpus()
	total := corpusBytes(events)

	mk := func(name string, make func(io.Writer) (Writer, error)) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(total)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var sink bytes.Buffer
				writer, err := make(&sink)
				if err != nil {
					b.Fatal(err)
				}
				for _, event := range events {
					if _, err := writer.Write(event); err != nil {
						b.Fatal(err)
					}
					if err := writer.Flush(); err != nil {
						b.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(events)), "ns/event")
		})
	}

	mk("br-lgwin22-q5", func(w io.Writer) (Writer, error) {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 5, LGWin: 22}), nil
	})
	mk("br-lgwin16-q4", func(w io.Writer) (Writer, error) {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 4, LGWin: 16}), nil
	})
	mk("zstd-win1M", func(w io.Writer) (Writer, error) {
		return zstd.NewWriter(w, zstd.WithWindowSize(1<<20), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
	})
	mk("zstd-win16K", func(w io.Writer) (Writer, error) {
		return zstd.NewWriter(w, zstd.WithWindowSize(16<<10), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
	})
	mk("zstd-win64K-crc", func(w io.Writer) (Writer, error) {
		return zstd.NewWriter(w, zstd.WithWindowSize(64<<10), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	})
}

// BenchmarkDecompressStream 上游方向的读路径: 解压完整压缩流。
func BenchmarkDecompressStream(b *testing.B) {
	events := sseCorpus()
	total := corpusBytes(events)

	// 各编码先压好一份完整流。
	compressed := map[string][]byte{}
	for _, encoding := range []string{Br, Zstd, Gzip, Deflate} {
		var sink bytes.Buffer
		writer, err := NewWriter(&sink, encoding)
		if err != nil {
			b.Fatal(err)
		}
		for _, event := range events {
			if _, err := writer.Write(event); err != nil {
				b.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			b.Fatal(err)
		}
		compressed[encoding] = sink.Bytes()
	}

	for _, encoding := range []string{Br, Zstd, Gzip, Deflate} {
		b.Run(encoding, func(b *testing.B) {
			data := compressed[encoding]
			b.ReportAllocs()
			b.SetBytes(total)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				reader, err := NewReader(bytes.NewReader(data), encoding)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, reader); err != nil {
					b.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestCompressionRatioOnSSECorpus 压缩比报告: 真实 SSE 语料上逐算法的流式压缩结果,
// 含当前参数与对照档位, 用于锁定 Preference 与 writer.go 常量。
func TestCompressionRatioOnSSECorpus(t *testing.T) {
	events := sseCorpus()
	total := corpusBytes(events)

	type row struct {
		name       string
		compressed int
	}
	var rows []row

	run := func(name string, make func(io.Writer) (Writer, error)) {
		var sink bytes.Buffer
		writer, err := make(&sink)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, event := range events {
			if _, err := writer.Write(event); err != nil {
				t.Fatal(err)
			}
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row{name, sink.Len()})
	}

	run("identity      ", func(w io.Writer) (Writer, error) { return NewWriter(w, Identity) })
	run("br  q5 lgwin16(当前)", func(w io.Writer) (Writer, error) { return NewWriter(w, Br) })
	run("br  q5 lgwin22", func(w io.Writer) (Writer, error) {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 5, LGWin: 22}), nil
	})
	run("br  q4 lgwin16", func(w io.Writer) (Writer, error) {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: 4, LGWin: 16}), nil
	})
	run("zstd 64K(当前)  ", func(w io.Writer) (Writer, error) { return NewWriter(w, Zstd) })
	run("zstd 1M        ", func(w io.Writer) (Writer, error) {
		return zstd.NewWriter(w, zstd.WithWindowSize(1<<20), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
	})
	run("zstd 16K       ", func(w io.Writer) (Writer, error) {
		return zstd.NewWriter(w, zstd.WithWindowSize(16<<10), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
	})
	run("gzip           ", func(w io.Writer) (Writer, error) { return NewWriter(w, Gzip) })
	run("deflate zlib   ", func(w io.Writer) (Writer, error) { return NewWriter(w, Deflate) })

	t.Logf("SSE 语料: %d 事件, 原始 %d 字节", len(events), total)
	for _, r := range rows {
		ratio := float64(total) / float64(r.compressed)
		t.Logf("%-22s %8d 字节  压缩比 %.2fx  (%.1f%%)", r.name, r.compressed, ratio, float64(r.compressed)/float64(total)*100)
	}
}
