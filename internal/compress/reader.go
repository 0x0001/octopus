package compress

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

// readCloser 串联解压器与底层流的关闭: 先释放解压器内部缓冲, 再关底层连接。
// gzip, flate, zlib, zstd 的解压端都要求显式 Close 才归还资源, 调用方只需 Close 一次。
type readCloser struct {
	io.Reader
	decoder io.Closer // 解压器自身, 实现 io.Closer 时登记(brotli 不需要)。
	source  io.Closer // 底层流, 实现 io.Closer 时登记(HTTP 响应体等)。
}

// newReadCloser 包装解压 Reader, 自动登记解压器与底层流各自的关闭入口。
func newReadCloser(decoder io.Reader, source io.Reader) *readCloser {
	rc := &readCloser{Reader: decoder}
	if closer, ok := decoder.(io.Closer); ok {
		rc.decoder = closer
	}
	if closer, ok := source.(io.Closer); ok {
		rc.source = closer
	}
	return rc
}

func (r *readCloser) Close() error {
	var err error
	if r.decoder != nil {
		if closeErr := r.decoder.Close(); closeErr != nil {
			err = closeErr
		}
	}
	if r.source != nil {
		if closeErr := r.source.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

// zstdCloser 把 zstd.Decoder 无返回值的 Close 适配成 io.Closer,
// 否则 newReadCloser 的类型断言会静默漏掉它, 解码 goroutine 滞留到 GC。
type zstdCloser struct{ *zstd.Decoder }

func (z *zstdCloser) Close() error {
	z.Decoder.Close()
	return nil
}

// newDeflateReader 嗅探 zlib 2 字节头(RFC 1950): CM 低 4 位必须为 8(deflate)
// 且 (CMF<<8|FLG) 能被 31 整除; 不匹配按历史遗留的裸 deflate 解, 剩余不足 2 字节按空流处理。
// 嗅探消耗的字节拼回流头, 对解压器不可见。
func newDeflateReader(r io.Reader) (io.ReadCloser, error) {
	header := make([]byte, 2)
	n, err := io.ReadFull(r, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	source := io.MultiReader(bytes.NewReader(header[:n]), r)
	if n < 2 {
		// 不足 zlib 头的流按 zlib/裸 deflate 都解不出数据, 把原字节透传 —— 它就是全部正文;
		// 登记底层关闭, 保证 HTTP 连接可正常归还。
		return newReadCloser(bytes.NewReader(header[:n]), r), nil
	}
	if isZlibHeader(header) {
		zr, err := zlib.NewReader(source)
		if err != nil {
			return nil, err
		}
		return newReadCloser(zr, r), nil
	}
	return newReadCloser(flate.NewReader(source), r), nil
}

// isZlibHeader 判断两字节是否符合 zlib 头校验。
func isZlibHeader(header []byte) bool {
	cmf, flg := uint16(header[0]), uint16(header[1])
	return header[0]&0x0f == 8 && (cmf<<8|flg)%31 == 0
}
