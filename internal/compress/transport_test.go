package compress

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// compressedHandler 返回以指定编码压缩 payload 的处理器, 用于验证客户端侧透明解压。
func compressedHandler(t *testing.T, encoding string, payload []byte) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", encoding)
		w.WriteHeader(http.StatusOK)
		writer, err := NewWriter(w, encoding)
		if err != nil {
			t.Errorf("NewWriter: %v", err)
			return
		}
		if _, err := writer.Write(payload); err != nil {
			t.Errorf("write: %v", err)
			return
		}
		if err := writer.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
}

// TestTransportUnwrapsAllEncodings 注入协商头且逐算法透明解压, 头与长度同步修正。
func TestTransportUnwrapsAllEncodings(t *testing.T) {
	t.Parallel()
	const payload = `{"choices":[{"delta":{"content":"compressed payload"}}]}`

	for _, encoding := range Preference {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(compressedHandler(t, encoding, []byte(payload)))
			defer server.Close()

			client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
			response, err := client.Post(server.URL, "application/json", strings.NewReader(payload))
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer response.Body.Close()

			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != payload {
				t.Fatalf("body = %q, %v; want %q", body, err, payload)
			}
			if response.Uncompressed != true {
				t.Error("Uncompressed = false, want true")
			}
			if response.ContentLength != -1 {
				t.Errorf("ContentLength = %d, want -1", response.ContentLength)
			}
			if response.Header.Get("Content-Encoding") != "" {
				t.Error("Content-Encoding header not stripped")
			}
			if _, ok := response.Header["Content-Length"]; ok {
				t.Error("Content-Length header not stripped")
			}
		})
	}
}

// TestTransportInjectsPreferenceHeader 上游必须收到按服务端偏好序拼接的协商头。
func TestTransportInjectsPreferenceHeader(t *testing.T) {
	t.Parallel()
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Accept-Encoding")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
	if _, err := client.Get(server.URL); err != nil {
		t.Fatalf("get: %v", err)
	}
	if want := "gzip, zstd, deflate, br"; seen != want {
		t.Fatalf("Accept-Encoding = %q, want %q", seen, want)
	}
}

// TestTransportRespectsExplicitHeader 调用方自带协商头时不覆盖也不解压, 保持透明。
func TestTransportRespectsExplicitHeader(t *testing.T) {
	t.Parallel()
	const payload = "explicit negotiation"
	server := httptest.NewServer(compressedHandler(t, Gzip, []byte(payload)))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()

	if response.Uncompressed {
		t.Error("Uncompressed = true, want false for explicit header")
	}
	if response.Header.Get("Content-Encoding") != Gzip {
		t.Error("Content-Encoding header should be preserved")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || bytes.Equal(body, []byte(payload)) {
		t.Fatalf("body should stay compressed, got %q, %v", body, err)
	}
}

// TestTransportSkipsBodylessResponses HEAD 与 204 响应没有可解压的正文, 头必须原样保留。
func TestTransportSkipsBodylessResponses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		method string
		status int
	}{
		"head": {http.MethodHead, http.StatusOK},
		"204":  {http.MethodGet, http.StatusNoContent},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", Gzip)
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
			request, err := http.NewRequest(tc.method, server.URL, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer response.Body.Close()
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if response.Header.Get("Content-Encoding") != Gzip {
				t.Error("Content-Encoding header should be preserved")
			}
			if response.Uncompressed {
				t.Error("Uncompressed = true, want false")
			}
		})
	}
}

// TestTransportUnwrapsErrorStatus 非成功状态的上游错误正文同样透明解压,
// 上层记录失败原因时直接拿到明文。
func TestTransportUnwrapsErrorStatus(t *testing.T) {
	t.Parallel()
	const payload = `{"error":{"message":"quota exceeded"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", Zstd)
		w.WriteHeader(http.StatusTooManyRequests)
		writer, err := NewWriter(w, Zstd)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if _, err := writer.Write([]byte(payload)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != payload {
		t.Fatalf("body = %q, %v; want %q", body, err, payload)
	}
}

// TestTransportStreamsFrames 上游逐帧 Flush 的压缩响应, 客户端在流结束前就能读到已发出帧,
// 帧间时序由 channel 串接: 读不到首帧则双方互等, 测试超时暴露阻塞。
func TestTransportStreamsFrames(t *testing.T) {
	t.Parallel()
	for _, encoding := range Preference {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			firstWritten := make(chan struct{})
			firstRead := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", encoding)
				w.WriteHeader(http.StatusOK)
				writer, err := NewWriter(w, encoding)
				if err != nil {
					t.Errorf("NewWriter: %v", err)
					return
				}
				if _, err := writer.Write([]byte("data: frame one\n\n")); err != nil {
					t.Errorf("write first: %v", err)
					return
				}
				if err := writer.Flush(); err != nil {
					t.Errorf("flush: %v", err)
					return
				}
				// 压缩器 Flush 只输出压缩缓冲, 还要把 http 层的网络缓冲一并发出。
				w.(http.Flusher).Flush()
				close(firstWritten)
				<-firstRead // 等客户端确认读到首帧后再发第二帧。
				if _, err := writer.Write([]byte("data: frame two\n\n")); err != nil {
					t.Errorf("write second: %v", err)
					return
				}
				if err := writer.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
			}))
			defer server.Close()

			client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
			response, err := client.Get(server.URL)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer response.Body.Close()

			<-firstWritten
			prefix := make([]byte, len("data: frame one\n\n"))
			if _, err := io.ReadFull(response.Body, prefix); err != nil || string(prefix) != "data: frame one\n\n" {
				t.Fatalf("first frame = %q, %v", prefix, err)
			}
			close(firstRead)

			rest, err := io.ReadAll(response.Body)
			if err != nil || string(rest) != "data: frame two\n\n" {
				t.Fatalf("second frame = %q, %v", rest, err)
			}
		})
	}
}

// TestTransportPassesThroughUnknownEncoding 上游声明我们不认识的编码时原样透传,
// 不吞正文也不猜编码, 异常由保留的头暴露。
func TestTransportPassesThroughUnknownEncoding(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "xz")
		_, _ = w.Write([]byte("opaque bytes"))
	}))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "opaque bytes" {
		t.Fatalf("body = %q, %v", body, err)
	}
	if response.Header.Get("Content-Encoding") != "xz" {
		t.Error("unknown Content-Encoding should be preserved")
	}
	if response.ContentLength != int64(len("opaque bytes")) {
		t.Errorf("ContentLength = %d, want untouched", response.ContentLength)
	}
}

// TestTransportFailsOnBrokenDeclaredEncoding 上游声明 gzip 实际不是 gzip 时显式失败:
// 解压器构造预读的流头字节无法归还, 透传会静默损坏正文, 必须走请求错误路径。
func TestTransportFailsOnBrokenDeclaredEncoding(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte("plain text, not gzip"))
	}))
	defer server.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
	response, err := client.Get(server.URL)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("want error for broken declared gzip, got response")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("error should mention the declared encoding: %v", err)
	}
}

// TestTransportNilBase 退回默认传输, 冒烟验证。
func TestTransportNilBase(t *testing.T) {
	t.Parallel()
	if NewTransport(nil) == nil {
		t.Fatal("NewTransport(nil) = nil")
	}
	if _, ok := NewTransport(nil).(interface {
		RoundTrip(*http.Request) (*http.Response, error)
	}); !ok {
		t.Fatal("NewTransport(nil) does not implement RoundTripper")
	}
}
