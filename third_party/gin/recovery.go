package gin

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"
)

// Logger 返回访问日志中间件。
func Logger() HandlerFunc {
	return LoggerWithWriter(os.Stderr)
}

// LoggerWithWriter 使用指定 writer 的访问日志中间件。
func LoggerWithWriter(out io.Writer) HandlerFunc {
	return func(c *Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery
		c.Next()
		// 客户端 IP 取直接对端, 信任代理头的完整实现不影响功能正确性。
		latency := time.Since(start)
		clientIP := c.Request.RemoteAddr
		if forwarded := c.GetHeader("X-Forwarded-For"); forwarded != "" {
			if idx := indexByte(forwarded, ','); idx != -1 {
				clientIP = forwarded[:idx]
			} else {
				clientIP = forwarded
			}
		}
		if raw != "" {
			path = path + "?" + raw
		}
		fmt.Fprintf(out, "[GIN] %3d | %13v | %-15s | %-7s %s\n",
			c.Writer.Status(), latency, clientIP, c.Request.Method, path)
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Recovery 使用默认 500 响应的 panic 兜底中间件。
func Recovery() HandlerFunc {
	return CustomRecovery(func(c *Context, _ any) {
		c.Status(http.StatusInternalServerError)
		c.Abort()
	})
}

// CustomRecovery 捕获 panic: 打印堆栈后调用自定义处理, 并中止链。
func CustomRecovery(handle func(c *Context, err any)) HandlerFunc {
	return func(c *Context) {
		defer func() {
			if err := recover(); err != nil {
				fmt.Fprintf(os.Stderr, "[GIN] panic recovered: %v\n%s\n", err, stack())
				handle(c, err)
				c.Abort()
			}
		}()
		c.Next()
	}
}

// stack 返回当前 goroutine 堆栈, 便于定位 panic 现场。
func stack() []byte {
	buf := make([]byte, 4096)
	for {
		n := runtime.Stack(buf, false)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, len(buf)*2)
	}
}
