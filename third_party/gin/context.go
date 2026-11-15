package gin

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
)

// ResponseWriter 包装标准库 ResponseWriter。
// 懒发头 —— WriteHeader 只记录状态, 首次 Write 或 WriteHeaderNow
// 才把头真正发出, 因此在写出前 handler 仍可修改任意响应头(SSE 中间件依赖)。
type ResponseWriter interface {
	http.ResponseWriter

	// Status 返回已记录(或即将发出)的状态码。
	Status() int
	// Size 返回已写出的字节数; 未写出时返回 -1。
	Size() int
	// WriteString 以字符串形式写入响应体。
	WriteString(string) (int, error)
	// Written 报告响应体是否已开始写出(响应头已发出)。
	Written() bool
	// WriteHeaderNow 立即发出已记录的状态头, 无正文也生效。
	WriteHeaderNow()
	// Hijack 接管底层连接(WebSocket 等), 与 http.Hijacker 对齐。
	Hijack() (net.Conn, *bufio.ReadWriter, error)
	// CloseNotify 转发 http.CloseNotifier, 连接断开时通知。
	CloseNotify() <-chan bool
	// Flush 推进网络缓冲, 实现 http.Flusher。
	Flush()
	// Pusher 转发 http.Pusher(HTTP/2 server push)。
	Pusher() http.Pusher
}

// responseWriter 是 ResponseWriter 的标准库实现。
type responseWriter struct {
	http.ResponseWriter
	size   int
	status int
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, size: -1, status: http.StatusOK}
}

// WriteHeader 记录状态码; 响应已写出后再改状态属于编程错误, 直接 panic。
func (w *responseWriter) WriteHeader(code int) {
	if code > 0 && w.status != code {
		if w.Written() {
			panic("gin: response headers were already written")
		}
		w.status = code
	}
}

// WriteHeaderNow 立即发出已记录的状态头。
func (w *responseWriter) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
		w.ResponseWriter.WriteHeader(w.status)
	}
}

func (w *responseWriter) Write(data []byte) (n int, err error) {
	w.WriteHeaderNow()
	n, err = w.ResponseWriter.Write(data)
	w.size += n
	return
}

func (w *responseWriter) WriteString(s string) (n int, err error) {
	w.WriteHeaderNow()
	n, err = io.WriteString(w.ResponseWriter, s)
	w.size += n
	return
}

func (w *responseWriter) Status() int   { return w.status }
func (w *responseWriter) Size() int     { return w.size }
func (w *responseWriter) Written() bool { return w.size != -1 }

func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.size < 0 {
		w.size = 0
	}
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("gin: the ResponseWriter does not support hijacking")
	}
	return hijacker.Hijack()
}

// CloseNotify 转发底层的 http.CloseNotifier(server 端仍实现, 已从公共接口移除)。
func (w *responseWriter) CloseNotify() <-chan bool {
	notifier, ok := w.ResponseWriter.(http.CloseNotifier)
	if !ok {
		panic("gin: the ResponseWriter does not support CloseNotify")
	}
	return notifier.CloseNotify()
}

func (w *responseWriter) Flush() {
	w.WriteHeaderNow()
	flusher, ok := w.ResponseWriter.(http.Flusher)
	if ok {
		flusher.Flush()
	}
}

func (w *responseWriter) Pusher() (pusher http.Pusher) {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher
	}
	return nil
}

// Context 携带请求与响应的贯穿上下文。
// 实例经 sync.Pool 复用, handler 不得在返回后继续持有。
type Context struct {
	Request *http.Request
	Writer  ResponseWriter

	handlers []HandlerFunc
	index    int
	aborted  bool
	keys     map[string]any
}

// contextPool 复用每请求的 Context, 消除热路径上的堆分配。
var contextPool = sync.Pool{New: func() any { return new(Context) }}

func acquireContext() *Context { return contextPool.Get().(*Context) }

func releaseContext(c *Context) {
	c.reset(nil, nil, nil)
	contextPool.Put(c)
}

// reset 归零复用字段; keys 的 map 保留容量供后续请求继续使用。
func (c *Context) reset(request *http.Request, writer ResponseWriter, handlers []HandlerFunc) {
	c.Request = request
	c.Writer = writer
	c.handlers = handlers
	c.index = -1
	c.aborted = false
	for k := range c.keys {
		delete(c.keys, k)
	}
}

// Next 推进处理链: 引擎入口调用一次, 循环执行到链尾或 Abort。
// 中间件在 Next 之后仍可执行收尾逻辑(defer 场景)。
// 中止用独立布尔标志而非"下标越过链尾"表示: 中间件可以在链推进之前
// (自己未调 Next 时)Abort, 此时若靠极大下标表示中止, 循环内的 index++
// 会从极大值溢出为负, 引发下标越界。
func (c *Context) Next() {
	c.index++
	for c.index < len(c.handlers) {
		if c.aborted {
			return
		}
		c.handlers[c.index](c)
		c.index++
	}
}

// Abort 中止链推进: 当前 handler 返回后, 后续 handler 不再执行。
// 对响应无副作用, 仅控制链。
func (c *Context) Abort() { c.aborted = true }

// IsAborted 报告链是否已被中止。
func (c *Context) IsAborted() bool { return c.aborted }

// Header 设置响应头(覆盖语义)。
func (c *Context) Header(key, value string) { c.Writer.Header().Set(key, value) }

// GetHeader 返回请求头的第一个值。
func (c *Context) GetHeader(key string) string { return c.Request.Header.Get(key) }

// Param 返回路由参数 :name 的值; 未匹配返回空串。
func (c *Context) Param(name string) string { return c.Request.PathValue(name) }

// Query 返回 URL 查询参数。
func (c *Context) Query(name string) string { return c.Request.URL.Query().Get(name) }

// Cookie 返回请求 Cookie 的值。
func (c *Context) Cookie(name string) (string, error) {
	cookie, err := c.Request.Cookie(name)
	if err != nil {
		return "", err
	}
	return cookie.Value, nil
}

// SetCookie 写 Set-Cookie 响应头。
func (c *Context) SetCookie(name, value string, maxAge int, path, domain string, secure, httpOnly bool) {
	if path == "" {
		path = "/"
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		MaxAge:   maxAge,
		Path:     path,
		Domain:   domain,
		Secure:   secure,
		HttpOnly: httpOnly,
	})
}

// Status 记录响应状态码(懒发头语义下首次 Write 时才真正发出)。
func (c *Context) Status(code int) { c.Writer.WriteHeader(code) }

// Set 在请求作用域存储键值。
func (c *Context) Set(key string, value any) {
	if c.keys == nil {
		c.keys = make(map[string]any)
	}
	c.keys[key] = value
}

// Get 取请求作用域的键值。
func (c *Context) Get(key string) (any, bool) {
	value, ok := c.keys[key]
	return value, ok
}

// GetInt 取请求作用域的 int 键值; 缺失或类型不符返回 0。
func (c *Context) GetInt(key string) int {
	if value, ok := c.Get(key); ok && value != nil {
		if v, ok := value.(int); ok {
			return v
		}
	}
	return 0
}

// NewResponseWriter 供测试构造 ResponseWriter 包装。
func NewResponseWriter(w http.ResponseWriter) ResponseWriter { return newResponseWriter(w) }
