package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/bestruirui/octopus/internal/compress"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"
)

// mountSSE 把 SSECompress 中间件与 handler 组链后挂到引擎的指定方法路径;
// pattern 形如 "GET /sse", 与 http.ServeMux 的注册写法一致。
// SSECompress 固定在链头, 与生产路由的挂载方式一致。
func mountSSE(engine *gin.Engine, pattern string, handlers ...gin.HandlerFunc) {
	method, path, _ := strings.Cut(pattern, " ")
	chain := append([]gin.HandlerFunc{SSECompress()}, handlers...)
	router := engine.Group("")
	switch method {
	case http.MethodPost:
		router.POST(path, chain...)
	default:
		router.GET(path, chain...)
	}
}

func newSSETestServer() *httptest.Server {

	engine := gin.New()
	router := engine
	mountSSE(router, "GET /sse", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.String(http.StatusOK, "data: hello\n\n")
	})
	mountSSE(router, "GET /json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(`{"ok":true}`))
	})
	return httptest.NewServer(router)
}

// rawClient 禁用自动压缩协商的客户端: 显式声明能力并自行解压,
// 用于断言响应线上的原始字节(自动协商的客户端会解压并剥头, 无法检查头)。
func rawClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}}
}

// openSSE 以指定 Accept-Encoding 请求 SSE 端点。
func openSSE(t *testing.T, client *http.Client, url string, acceptEncoding string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if acceptEncoding != "" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return response
}

// decodeReader 用各算法的独立实现构造解压 Reader, 交叉验证服务端输出是标准格式
// (gzip/deflate 用标准库, br/zstd 用第三方库, 均不经过本包的读取路径)。
func decodeReader(t *testing.T, encoding string, data io.Reader) io.ReadCloser {
	t.Helper()
	switch encoding {
	case compress.Gzip:
		gr, err := gzip.NewReader(data)
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		return gr
	case compress.Deflate:
		zr, err := zlib.NewReader(data)
		if err != nil {
			t.Fatalf("zlib.NewReader: %v", err)
		}
		return zr
	case compress.Br:
		// brotli.Reader 无需关闭, NopCloser 补齐接口。
		return io.NopCloser(brotli.NewReader(data))
	case compress.Zstd:
		zr, err := zstd.NewReader(data, zstd.WithDecoderConcurrency(1))
		if err != nil {
			t.Fatalf("zstd.NewReader: %v", err)
		}
		// Decoder 的 Close 无返回值不满足 io.Closer, 测试短生命周期不依赖它。
		return io.NopCloser(zr)
	default:
		t.Fatalf("unsupported encoding %q", encoding)
		return nil
	}
}

// decodeIndependent 解压完整字节串。
func decodeIndependent(t *testing.T, encoding string, data []byte) string {
	t.Helper()
	reader := decodeReader(t, encoding, bytes.NewReader(data))
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return string(decoded)
}

// TestSSECompressNegotiatesAllEncodings SSE 响应按客户端能力逐算法压缩,
// 头部补 Content-Encoding 与 Vary, 输出格式经独立实现交叉验证。
func TestSSECompressNegotiatesAllEncodings(t *testing.T) {
	t.Parallel()
	const payload = "data: hello\n\n"

	for _, encoding := range compress.Preference {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			// 生命周期: 并行子测试在父函数 defer 之后才恢复执行, server 必须留在子测试内。
			server := newSSETestServer()
			defer server.Close()

			response := openSSE(t, rawClient(), server.URL+"/sse", encoding)
			defer response.Body.Close()

			if got := response.Header.Get("Content-Encoding"); got != encoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, encoding)
			}
			if vary := response.Header.Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
				t.Fatalf("Vary = %q, want Accept-Encoding included", vary)
			}
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if bytes.Equal(raw, []byte(payload)) {
				t.Fatal("body should be compressed")
			}
			if decoded := decodeIndependent(t, encoding, raw); decoded != payload {
				t.Fatalf("decoded = %q, want %q", decoded, payload)
			}
		})
	}
}

// TestSSECompressSkipsNonStreamContent JSON 响应不压缩, 头与正文保持原样。
func TestSSECompressSkipsNonStreamContent(t *testing.T) {
	t.Parallel()
	server := newSSETestServer()
	defer server.Close()

	response := openSSE(t, rawClient(), server.URL+"/json", "gzip, deflate, br")
	defer response.Body.Close()

	if response.Header.Get("Content-Encoding") != "" {
		t.Error("JSON response should not be compressed")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("body = %q, %v; want plaintext", body, err)
	}
}

// TestSSECompressSkipsUnsupportedClients 客户端未声明或不接受任何压缩编码时透传。
func TestSSECompressSkipsUnsupportedClients(t *testing.T) {
	t.Parallel()
	const payload = "data: hello\n\n"

	for name, header := range map[string]string{
		"header missing": "",
		"all rejected":   "br;q=0, zstd;q=0, gzip;q=0, deflate;q=0",
		"zero wildcard":  "*;q=0",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := newSSETestServer()
			defer server.Close()

			response := openSSE(t, rawClient(), server.URL+"/sse", header)
			defer response.Body.Close()

			if response.Header.Get("Content-Encoding") != "" {
				t.Errorf("Content-Encoding = %q, want passthrough", response.Header.Get("Content-Encoding"))
			}
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != payload {
				t.Fatalf("body = %q, %v; want plaintext", body, err)
			}
		})
	}
}

// TestSSECompressStreamsFrames 逐帧实时投递: 第二帧在客户端确认读到第一帧之后才写,
// 客户端若不能在流结束前读到第一帧, channel 互等会让测试超时暴露阻塞。
func TestSSECompressStreamsFrames(t *testing.T) {
	t.Parallel()
	for _, encoding := range compress.Preference {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()

			engine := gin.New()
			router := engine
			frame1Read := make(chan struct{})
			mountSSE(router, "GET /sse", func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				if _, err := c.Writer.Write([]byte("data: frame one\n\n")); err != nil {
					t.Errorf("write first: %v", err)
					return
				}
				c.Writer.Flush()
				<-frame1Read // 等客户端确认读到第一帧。
				if _, err := c.Writer.Write([]byte("data: frame two\n\n")); err != nil {
					t.Errorf("write second: %v", err)
				}
			})
			server := httptest.NewServer(router)
			defer server.Close()

			response := openSSE(t, rawClient(), server.URL+"/sse", encoding)
			defer response.Body.Close()
			reader := decodeReader(t, encoding, response.Body)
			defer reader.Close()

			prefix := make([]byte, len("data: frame one\n\n"))
			if _, err := io.ReadFull(reader, prefix); err != nil || string(prefix) != "data: frame one\n\n" {
				t.Fatalf("first frame = %q, %v", prefix, err)
			}
			close(frame1Read)

			rest, err := io.ReadAll(reader)
			if err != nil || string(rest) != "data: frame two\n\n" {
				t.Fatalf("second frame = %q, %v", rest, err)
			}
		})
	}
}

// TestSSECompressSmallFrameLatency 小帧到达延迟量化: SSE 单帧只有二十来字节,
// 压缩器/网络层任何一层滞留缓冲, 客户端都要等到流结束才能读到 —— handler 写完一帧
// 后不再写任何数据也不结束流, 客户端读不到就互等超时。断言给足 CI 抖动余量,
// 正常情形应在数十毫秒内到达。
func TestSSECompressSmallFrameLatency(t *testing.T) {
	t.Parallel()
	const frame = "data: {" + `"id":"chatcmpl-tiny"` + "}\n\n" // 28 字节, 远小于任何压缩缓冲。

	for _, encoding := range compress.Preference {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()

			engine := gin.New()
			router := engine
			frameRead := make(chan struct{})
			mountSSE(router, "GET /sse", func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				if _, err := c.Writer.Write([]byte(frame)); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				c.Writer.Flush()
				<-frameRead // 一帧后不再写也不结束, 滞留缓冲立即死锁暴露。
			})
			server := httptest.NewServer(router)
			defer server.Close()

			response := openSSE(t, rawClient(), server.URL+"/sse", encoding)
			defer response.Body.Close()
			reader := decodeReader(t, encoding, response.Body)
			defer reader.Close()

			start := time.Now()
			got := make([]byte, len(frame))
			if _, err := io.ReadFull(reader, got); err != nil || string(got) != frame {
				t.Fatalf("frame = %q, %v; want %q", got, err, frame)
			}
			elapsed := time.Since(start)
			close(frameRead)
			// 滞留缓冲时读端要等到互等超时(秒级); 正常 flush 到达应在数十毫秒。
			if elapsed > 2*time.Second {
				t.Fatalf("first small frame took %v, flush must be immediate", elapsed)
			}
			t.Logf("%s: 28-byte frame reached client in %v", encoding, elapsed)
		})
	}
}

// TestSSECompressGoSDKAutoGzip 模拟 Go SDK 真实行为: net/http 默认客户端自动声明
// gzip 并自动解压, 端到端拿到明文 SSE。
func TestSSECompressGoSDKAutoGzip(t *testing.T) {
	t.Parallel()
	server := newSSETestServer()
	defer server.Close()

	response, err := http.Get(server.URL + "/sse") // 默认客户端, 自动协商 gzip。
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "data: hello\n\n" {
		t.Fatalf("body = %q, %v; want plaintext", body, err)
	}
	if !response.Uncompressed {
		t.Error("Uncompressed = false, transport should auto-decompress")
	}
}

// TestSSECompressEndToEndRelay 完整链路: 模拟上游(gzip 压缩 SSE 的 LLM 供应商)
// -> relay(阶段 2 透明解压上游, 阶段 3 再压缩给下游) -> 下游(声明 br 的客户端,
// 独立实现解压验证)。帧间时序用 channel 串接, 任何一环滞留都会超时暴露。
func TestSSECompressEndToEndRelay(t *testing.T) {
	t.Parallel()
	const (
		frameOne = "data: frame one\n\n"
		frameTwo = "data: frame two\n\n"
	)
	frame1Written := make(chan struct{})
	frame1Acked := make(chan struct{})
	frame2Allowed := make(chan struct{})

	// 模拟上游: gzip 压缩 SSE, 逐帧 flush。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", compress.Gzip)
		w.WriteHeader(http.StatusOK)
		gw := gzip.NewWriter(w)
		if _, err := gw.Write([]byte(frameOne)); err != nil {
			t.Errorf("write frame one: %v", err)
			return
		}
		if err := gw.Flush(); err != nil {
			t.Errorf("flush: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		close(frame1Written)
		<-frame2Allowed // 等下游确认收到帧一。
		if _, err := gw.Write([]byte(frameTwo)); err != nil {
			t.Errorf("write frame two: %v", err)
			return
		}
		if err := gw.Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		if err := gw.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}))
	defer upstream.Close()

	// 模拟 octopus relay: 经阶段 2 的 Transport 透明解压上游, 逐事件转发时被阶段 3 压缩。

	relayEngine := gin.New()
	relayRouter := relayEngine
	mountSSE(relayRouter, "POST /v1/chat/completions", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		client := &http.Client{Transport: compress.NewTransport(http.DefaultTransport)}
		response, err := client.Post(upstream.URL, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Errorf("relay upstream: %v", err)
			return
		}
		defer response.Body.Close()

		if _, err := io.ReadFull(response.Body, make([]byte, len(frameOne))); err != nil {
			t.Errorf("read frame one: %v", err)
			return
		}
		if _, err := c.Writer.Write([]byte(frameOne)); err != nil {
			t.Errorf("forward frame one: %v", err)
			return
		}
		c.Writer.Flush()
		<-frame1Acked // 等下游确认读到帧一, 证明逐帧实时推进。
		close(frame2Allowed)

		if _, err := io.ReadFull(response.Body, make([]byte, len(frameTwo))); err != nil {
			t.Errorf("read frame two: %v", err)
			return
		}
		if _, err := c.Writer.Write([]byte(frameTwo)); err != nil {
			t.Errorf("forward frame two: %v", err)
		}
	})
	relay := httptest.NewServer(relayRouter)
	defer relay.Close()

	// 下游: 声明 br, 自行解压并检查原始头。
	downstream := rawClient()
	request, err := http.NewRequest(http.MethodPost, relay.URL+"/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Accept-Encoding", compress.Br)
	response, err := downstream.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()

	if got := response.Header.Get("Content-Encoding"); got != compress.Br {
		t.Fatalf("Content-Encoding = %q, want %q", got, compress.Br)
	}
	reader := brotli.NewReader(response.Body)

	prefix := make([]byte, len(frameOne))
	if _, err := io.ReadFull(reader, prefix); err != nil || string(prefix) != frameOne {
		t.Fatalf("first frame = %q, %v; want %q", prefix, err, frameOne)
	}
	close(frame1Acked)

	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != frameTwo {
		t.Fatalf("second frame = %q, %v; want %q", rest, err, frameTwo)
	}
}

// TestSSECompressHijackTerminatesCompressor 连接被接管(websocket 场景)前终结压缩器,
// 不 panic 即可。
func TestSSECompressHijackTerminatesCompressor(t *testing.T) {
	t.Parallel()

	engine := gin.New()
	router := engine
	mountSSE(router, "GET /sse", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		if _, err := c.Writer.Write([]byte("data: hello\n\n")); err != nil {
			t.Errorf("write: %v", err)
			return
		}
		if _, ok := c.Writer.(http.Hijacker); !ok {
			t.Error("wrapped writer should stay a Hijacker")
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()

	response := openSSE(t, rawClient(), server.URL+"/sse", compress.Gzip)
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// TestSSECompressRejectsWriteAfterCompletion 请求结束后写入必须显式报错:
// 压缩流尾已写, 静默丢弃会让持 writer 的误用(goroutine 泄漏等)无从排查。
func TestSSECompressRejectsWriteAfterCompletion(t *testing.T) {
	t.Parallel()

	engine := gin.New()
	router := engine
	finishSignal := make(chan struct{})
	lateWriteDone := make(chan struct{})
	var postWriteErr error
	mountSSE(router, "GET /sse", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		writer := c.Writer // 模拟误用方持有 writer 引用, 而非随请求回收的 Context。
		if _, err := writer.Write([]byte("data: first\n\n")); err != nil {
			t.Errorf("write: %v", err)
		}
		// handler 返回触发 finish 后, 模拟误用方继续写入。
		go func() {
			<-finishSignal
			_, postWriteErr = writer.Write([]byte("data: late\n\n"))
			close(lateWriteDone)
		}()
		close(finishSignal)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	response := openSSE(t, rawClient(), server.URL+"/sse", compress.Gzip)
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	<-lateWriteDone
	if postWriteErr == nil {
		t.Fatal("write after completion should fail explicitly")
	}
}
