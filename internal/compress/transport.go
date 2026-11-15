package compress

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// transport 包装出站请求, 主动与上游协商压缩并透明解压响应。
// 请求未携带 Accept-Encoding 时注入服务端偏好序并接管响应解压;
// 调用方已自带协商头时完全不干预, 行为与裸 RoundTripper 一致。
// 透传(client.Do)与 pipeline(DoStream)共用同一 http.Client, 在 Transport 层包装即全覆盖,
// 上游错误正文同样被解压, 上层记录的错误日志直接可读。
type transport struct {
	base http.RoundTripper
}

// NewTransport 在 base 之上包一层压缩协商; base 为 nil 时退回 http.DefaultTransport。
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	outbound := req
	negotiated := false
	if req.Header.Get("Accept-Encoding") == "" {
		// RoundTrip 契约禁止修改传入请求, 注入协商头须经深拷贝; 正文只引用不复制。
		outbound = req.Clone(req.Context())
		outbound.Header.Set("Accept-Encoding", strings.Join(Preference, ", "))
		negotiated = true
	}

	response, err := t.base.RoundTrip(outbound)
	if err != nil {
		return nil, err
	}
	// 只在协商头由本层注入时接管解压: 调用方自带协商头意味着压缩语义由它自己负责。
	if negotiated {
		if err := unwrapResponse(response); err != nil {
			return nil, err
		}
	}
	return response, nil
}

// unwrapResponse 按响应声明的编码透明解压正文并修正头与长度;
// 无正文的响应(HEAD/204/304)与未声明压缩的响应原样保留。
// gzip 与 deflate 的解压器构造会急切读取流头, 相当于首字节多等几个字节,
// 对流式响应而言一个 SSE 事件远大于这个量级, 无感知差异。
func unwrapResponse(response *http.Response) error {
	encoding := response.Header.Get("Content-Encoding")
	if response.Body == nil || response.Request == nil || response.Request.Method == http.MethodHead ||
		response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotModified ||
		encoding == "" || normalize(encoding) == Identity {
		return nil
	}
	reader, err := NewReader(response.Body, encoding)
	if err != nil {
		if errors.Is(err, ErrUnsupportedEncoding) {
			// 不认识的编码: 底层流未被消费, 原样透传, 压缩语义交还客户端与上游自行处理。
			return nil
		}
		// 声明的编码建解压器失败(如 gzip 实际不是 gzip): 构造时可能已预读流头,
		// 字节无法归还, 透传会静默损坏正文 —— 关闭响应并显式失败, 交上层走请求错误路径。
		response.Body.Close()
		return fmt.Errorf("compress: upstream declared %q but body is not decodable: %w", encoding, err)
	}
	response.Body = reader
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	response.Uncompressed = true
	return nil
}
