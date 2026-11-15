package compress

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"io"
	"strings"
	"testing"
)

// TestRoundTrip 逐算法验证: 写入 -> Flush 后解压端立即可读(SSE 逐事件实时性的关键语义)
// -> 继续写入 -> Close 补写流尾(gzip/zstd 缺尾即流损坏) -> 全量读回一致。
// 读写两端用 io.Pipe 并发推进模拟真实网络: 内存共享 buffer 在流中间出现 EOF 后又续写,
// 会触发 zstd 解压端预读下一帧时的假性截断, 这不是线上读端的真实时序。
func TestRoundTrip(t *testing.T) {
	t.Parallel()
	for _, encoding := range []string{Identity, Gzip, Deflate, Br, Zstd} {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			pipeReader, pipeWriter := io.Pipe()
			writer, err := NewWriter(pipeWriter, encoding)
			if err != nil {
				t.Fatalf("NewWriter(%s): %v", encoding, err)
			}
			// 写端必须先于 NewReader 启动: gzip/deflate 的解压器构造会急切读取流头,
			// 此时 pipe 上还没有任何写端, 同步管道会永久阻塞。
			writeErr := make(chan error, 1)
			go func() {
				defer close(writeErr)
				defer pipeWriter.Close() // 对应线上流结束时的连接关闭, 否则读端等不到 EOF。
				if _, err := writer.Write([]byte("hello ")); err != nil {
					writeErr <- err
					return
				}
				if err := writer.Flush(); err != nil {
					writeErr <- err
					return
				}
				if _, err := writer.Write([]byte("world")); err != nil {
					writeErr <- err
					return
				}
				writeErr <- writer.Close()
			}()
			reader, err := NewReader(pipeReader, encoding)
			if err != nil {
				t.Fatalf("NewReader(%s): %v", encoding, err)
			}

			prefix := make([]byte, 6)
			if _, err := io.ReadFull(reader, prefix); err != nil || string(prefix) != "hello " {
				t.Fatalf("after flush read = %q, %v; want %q", prefix, err, "hello ")
			}
			rest, err := io.ReadAll(reader)
			if err != nil || string(rest) != "world" {
				t.Fatalf("after close read = %q, %v; want %q", rest, err, "world")
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("close reader: %v", err)
			}
			if err := <-writeErr; err != nil {
				t.Fatalf("writer side: %v", err)
			}
		})
	}
}

// TestDeflateHeaderSniffing 验证 deflate 双格式: RFC 9110 定义的 zlib 格式与
// 历史遗留的裸 deflate 服务端实现都必须能解。
func TestDeflateHeaderSniffing(t *testing.T) {
	t.Parallel()
	const payload = "deflate payload, both framings must decode"

	zlibBuf := &bytes.Buffer{}
	zlibWriter := zlib.NewWriter(zlibBuf)
	if _, err := zlibWriter.Write([]byte(payload)); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zlibWriter.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}

	rawBuf := &bytes.Buffer{}
	rawWriter, _ := flate.NewWriter(rawBuf, flate.DefaultCompression)
	if _, err := rawWriter.Write([]byte(payload)); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := rawWriter.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}

	for name, data := range map[string][]byte{"zlib": zlibBuf.Bytes(), "raw": rawBuf.Bytes()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reader, err := NewReader(bytes.NewReader(data), Deflate)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			defer reader.Close()
			got, err := io.ReadAll(reader)
			if err != nil || string(got) != payload {
				t.Fatalf("decoded = %q, %v; want %q", got, err, payload)
			}
		})
	}
}

// TestEmptyAndTruncatedStreams 空流返回空, 不足 zlib 头的流把原字节透传(它就是全部正文)。
func TestEmptyAndTruncatedStreams(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		input string
		want  string
	}{
		"empty":    {input: "", want: ""},
		"one byte": {input: "\x78", want: "\x78"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reader, err := NewReader(strings.NewReader(tc.input), Deflate)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			defer reader.Close()
			got, err := io.ReadAll(reader)
			if err != nil || string(got) != tc.want {
				t.Fatalf("decoded = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// readCountingCloser 记录底层流的 Close 是否被透传。
type readCountingCloser struct {
	io.Reader
	closed bool
}

func (r *readCountingCloser) Close() error {
	r.closed = true
	return nil
}

// TestReaderCloseClosesSource 每种编码的解压 Reader 关闭时都要关掉底层流,
// 上游方向靠这一点在 Transport 包装后保持响应体正常归还连接。
func TestReaderCloseClosesSource(t *testing.T) {
	t.Parallel()
	for _, encoding := range []string{Identity, Gzip, Deflate, Br, Zstd} {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			// 用合法压缩数据构造, 避免解压器在首次读时因数据非法而提前出错。
			var buf bytes.Buffer
			writer, err := NewWriter(&buf, encoding)
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			if _, err := writer.Write([]byte("x")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			source := &readCountingCloser{Reader: bytes.NewReader(buf.Bytes())}
			reader, err := NewReader(source, encoding)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if !source.closed {
				t.Fatalf("%s: source not closed", encoding)
			}
		})
	}
}

// TestCompressedOutputDiffers 压缩编码的输出不能等于原文, 防止误配置成透传。
func TestCompressedOutputDiffers(t *testing.T) {
	t.Parallel()
	const payload = "the quick brown fox jumps over the lazy dog"
	for _, encoding := range []string{Gzip, Deflate, Br, Zstd} {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			writer, err := NewWriter(&buf, encoding)
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			if _, err := writer.Write([]byte(payload)); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if buf.String() == payload {
				t.Fatal("compressed output equals input")
			}
		})
	}
}

// TestUnsupportedEncoding 未知编码在读写两侧都必须报错, 不允许静默透传。
func TestUnsupportedEncoding(t *testing.T) {
	t.Parallel()
	if _, err := NewWriter(io.Discard, "bzip2"); err == nil {
		t.Fatal("NewWriter: want error for unsupported encoding")
	}
	if _, err := NewReader(bytes.NewReader(nil), "bzip2"); err == nil {
		t.Fatal("NewReader: want error for unsupported encoding")
	}
}
