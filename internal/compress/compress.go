// Package compress 为 HTTP 流量提供内容压缩支持:
// Accept-Encoding 协商(q 值/通配/拒绝语义), gzip, deflate, br, zstd 四种算法的
// 流式编解码(单流内存刻意偏小), 上游方向的自动压缩协商 RoundTripper。
//
// 两个接入方向互不依赖, 均只依赖本包的极简 API:
//   - 上游方向: NewTransport 包一层出站 http.Client 的 Transport, 主动协商并透明解压;
//   - 客户端方向: Negotiate + NewWriter 在响应首次写出前懒协商, 包装 SSE 输出。
package compress

import (
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// HTTP 内容编码 token; 按 RFC 9110 比较不区分大小写, 本包统一小写。
const (
	Identity = "identity" // 不压缩。
	Gzip     = "gzip"     // RFC 1952; 历史别名 x-gzip。
	Deflate  = "deflate"  // RFC 9110 定义为 zlib 格式, 兼容裸 deflate。
	Br       = "br"       // Brotli, RFC 7932。
	Zstd     = "zstd"     // RFC 8878。
)

// Preference 服务端支持的压缩编码偏好序, 协商总是选中客户端可接受的第一个;
// 调整顺序即可改变协商与上游注入的 Accept-Encoding。
// 顺序由基准数据锁定(真实 LLM SSE 语料, 逐事件 Write+Flush):
//
//	gzip: 压缩比 17.4x / 80MB/s / 3.4µs每事件 全维度最优, 且所有客户端都支持;
//	zstd: anthropic-python 实测声明, 客户端解码极快, 独立受众;
//	deflate: 压缩比与 gzip 同级但受众几乎被 gzip 覆盖;
//	br: 逐事件 flush 模式下最慢(10.8µs每事件)且压缩比低于 gzip, 仅作 br-only 客户端兑底。
var Preference = []string{Gzip, Zstd, Deflate, Br}

// Writer 是流式压缩编码器: 写入的数据经压缩流向底层 w。
// Flush 把已写数据全部输出但不终止压缩流(SSE 逐事件实时投递的关键);
// Close 终止压缩流(如 gzip/zstd 的校验尾)并释放资源, 不 Close 数据会丢失或流损坏。
type Writer interface {
	io.WriteCloser

	// Flush 输出全部已缓冲数据, 保证后续从解压端可读已写内容。
	Flush() error
}

// NewWriter 返回把数据按 encoding 压缩写入 w 的编码器。
// Identity 原样透传, x-gzip 视作 gzip, 未知编码返回错误。
func NewWriter(w io.Writer, encoding string) (Writer, error) {
	switch normalize(encoding) {
	case Identity:
		return identityWriter{w: w}, nil
	case Gzip:
		return gzip.NewWriterLevel(w, writerGzipLevel)
	case Deflate:
		// RFC 9110 定义 deflate 为 zlib 格式(RFC 1950), 服务端必须写 zlib 头;
		// 读端(NewReader)才做裸 deflate 嗅探以兼容历史服务器。
		return zlib.NewWriterLevel(w, writerDeflateLevel)
	case Br:
		return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: writerBrotliQuality, LGWin: writerBrotliLGWin}), nil
	case Zstd:
		return zstd.NewWriter(w,
			zstd.WithWindowSize(writerZstdWindowSize),
			zstd.WithEncoderConcurrency(1),
			zstd.WithEncoderCRC(false),
		)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedEncoding, encoding)
	}
}

// NewReader 返回把 r 按 encoding 解压的读取器。
// 返回值的 Close 依次关闭解压器与底层流(r 实现 io.Closer 时), 交给调用方负责。
// Deflate 按 RFC 9110 要求嗅探 zlib 头, 不匹配时回退裸 deflate; Identity 原样透传。
func NewReader(r io.Reader, encoding string) (io.ReadCloser, error) {
	switch normalize(encoding) {
	case Identity:
		// 透传但仍透传底层流的关闭, 否则响应体连接无法归还。
		if _, ok := r.(io.Closer); ok {
			return newReadCloser(r, nil), nil
		}
		return io.NopCloser(r), nil
	case Gzip:
		gr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("compress: gzip reader: %w", err)
		}
		return newReadCloser(gr, r), nil
	case Deflate:
		return newDeflateReader(r)
	case Br:
		return newReadCloser(brotli.NewReader(r), r), nil
	case Zstd:
		dr, err := zstd.NewReader(r,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
		)
		if err != nil {
			return nil, fmt.Errorf("compress: zstd reader: %w", err)
		}
		return newReadCloser(&zstdCloser{Decoder: dr}, r), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedEncoding, encoding)
	}
}

// ErrUnsupportedEncoding 编码超出本包支持范围; NewReader 返回它时底层流未被消费,
// 调用方可安全原样透传。其余错误(如声明的 gzip 实际不是 gzip)意味着解压器构造时
// 可能已预读流头, 字节无法归还, 调用方不应再透传该流。
var ErrUnsupportedEncoding = errors.New("compress: unsupported encoding")

// normalize 把内容编码 token 归一为规范名: 忽略大小写与首尾空白, x-gzip 别名映射到 gzip。
func normalize(encoding string) string {
	token := strings.ToLower(strings.TrimSpace(encoding))
	if token == "x-gzip" {
		return Gzip
	}
	return token
}

// identityWriter 不压缩只透传, 让调用方以统一接口处理协商出 Identity 的路径。
type identityWriter struct{ w io.Writer }

func (i identityWriter) Write(p []byte) (int, error) { return i.w.Write(p) }
func (identityWriter) Flush() error                  { return nil }
func (identityWriter) Close() error                  { return nil }
