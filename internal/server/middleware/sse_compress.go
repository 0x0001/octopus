package middleware

import (
	"bufio"
	"errors"
	"net"
	"strings"

	"github.com/bestruirui/octopus/internal/compress"
	"github.com/gin-gonic/gin"
)

// errWriteAfterDone 请求结束后继续写入(writer 被 goroutine 持有时)的错误:
// 压缩流尾已写, 再写既无法进入流也不可恢复, 显式报错优于静默丢弃。
var errWriteAfterDone = errors.New("sse_compress: writer used after request completion")

// sseContentType SSE 响应的内容类型前缀; 后面可能跟随 charset 参数。
const sseContentType = "text/event-stream"

// SSECompress 对 SSE 流式响应按客户端能力透明压缩。
// 懒协商: 首次写出前才检查响应的 Content-Type, 命中 text/event-stream 且客户端
// 接受压缩编码才包装; 管理 API 的 JSON 响应、不支持压缩的客户端零开销透传,
// 因此全局挂载也只影响流式响应。
// 每次事件的 Flush 同时推进压缩器与网络缓冲, 保证逐事件实时投递;
// 请求结束时补写压缩流尾(gzip/zstd 缺尾即流损坏)。
func SSECompress() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 协商结果在进入时确定, 与写出时机解耦; Identity 表示客户端不接受压缩。
		writer := &sseCompressWriter{
			ResponseWriter: c.Writer,
			accept:         compress.Negotiate(c.Request.Header.Get("Accept-Encoding")),
		}
		c.Writer = writer
		defer writer.finish()
		c.Next()
	}
}

// sseCompressWriter 包装响应写入器, 未覆写的方法(Status/Size/CloseNotify...)
// 经嵌入透传给原始写入器。
type sseCompressWriter struct {
	gin.ResponseWriter
	accept     string          // 客户端接受的压缩编码, 进入时已协商。
	compressor compress.Writer // 协商命中且响应为 SSE 时建立; nil 表示透传。
	settled    bool            // 懒协商是否已执行(命中与否都算执行过)。
	done       bool            // 请求已结束(finish 已执行); 当前所有 SSE handler 均为同步写, 此后写入报错。
}

// settle 懒协商: 首次写出前根据响应 Content-Type 与客户端能力决定是否压缩。
// 命中时补写 Content-Encoding 与 Vary, 必须赶在响应头发出之前。
func (w *sseCompressWriter) settle() {
	if w.settled {
		return
	}
	w.settled = true
	if w.accept == compress.Identity || !strings.HasPrefix(w.Header().Get("Content-Type"), sseContentType) {
		return
	}
	w.Header().Add("Vary", "Accept-Encoding")
	compressor, err := compress.NewWriter(w.ResponseWriter, string(w.accept))
	if err != nil {
		// 编码是协商产物, 构造失败仅可能来自配置错误: 放弃压缩, 降级透传。
		w.Header().Del("Vary")
		return
	}
	w.Header().Set("Content-Encoding", string(w.accept))
	w.compressor = compressor
}

func (w *sseCompressWriter) Write(data []byte) (int, error) {
	if w.done {
		return 0, errWriteAfterDone
	}
	w.settle()
	if w.compressor == nil {
		return w.ResponseWriter.Write(data)
	}
	// 返回值是调用方视角的明文字节数, 由压缩器承诺消费。
	return w.compressor.Write(data)
}

func (w *sseCompressWriter) WriteString(s string) (int, error) {
	if w.done {
		return 0, errWriteAfterDone
	}
	w.settle()
	if w.compressor == nil {
		return w.ResponseWriter.WriteString(s)
	}
	return w.compressor.Write([]byte(s))
}

// WriteHeaderNow 响应头发出的最后一道关口: 协商必须在此之前完成,
// Content-Encoding 才能随响应头一起生效。
func (w *sseCompressWriter) WriteHeaderNow() {
	w.settle()
	w.ResponseWriter.WriteHeaderNow()
}

// Flush 双层推进: 压缩器 Flush 只把缓冲交给网络层, 还需网络层 Flush 才真正发出,
// 缺任何一层事件都会滞留到缓冲满或流结束。
func (w *sseCompressWriter) Flush() {
	if w.compressor != nil {
		_ = w.compressor.Flush()
	}
	w.ResponseWriter.Flush()
}

// finish 请求结束: 补写压缩流尾并释放资源。响应头已发出, 失败无从补救, 忽略错误。
// 此后写入一律报错(见 errWriteAfterDone): 压缩流已终结, 后续字节无处安放。
// 警示: handler 不得把 c.Writer 交给 goroutine 在返回后使用。
func (w *sseCompressWriter) finish() {
	if w.done {
		return
	}
	w.done = true
	if w.compressor == nil {
		return
	}
	_ = w.compressor.Close()
	w.compressor = nil
}

// Hijack 前先终结压缩器: 底层连接被接管后压缩字节无处可写。
func (w *sseCompressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.finish()
	return w.ResponseWriter.Hijack()
}
