// Package cors 提供 CORS 中间件: 白名单动态求值、凭据与暴露头配置、
// 预检请求短路。
package cors

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// headerAccessControlRequestMethod 是预检请求的判定头。
const headerAccessControlRequestMethod = "Access-Control-Request-Method"

// Config 是中间件配置。
type Config struct {
	AllowAllOrigins  bool
	AllowOriginFunc  func(origin string) bool
	AllowCredentials bool
	AllowMethods     []string
	AllowHeaders     []string
	ExposeHeaders    []string
	MaxAge           time.Duration
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		AllowMethods: []string{
			http.MethodGet,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
			http.MethodHead,
			http.MethodOptions,
		},
		AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	}
}

// New 返回 CORS 中间件:
//   - 无 Origin 头的请求直接放行(非浏览器请求);
//   - 白名单未命中时既不加头也不短路, 请求继续走完后续链(预检则落到 404);
//   - 命中时普通请求回显 ACAO 并附加 Credentials/Expose-Headers;
//   - 命中的预检(OPTIONS + Access-Control-Request-Method)以 204 短路。
func New(config Config) gin.HandlerFunc {
	// 启动时归一化: 空值回落到 DefaultConfig 的对应默认值。
	defaults := DefaultConfig()
	if len(config.AllowMethods) == 0 {
		config.AllowMethods = defaults.AllowMethods
	}
	if len(config.AllowHeaders) == 0 {
		config.AllowHeaders = defaults.AllowHeaders
	}
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin == "" {
			c.Next()
			return
		}
		if !config.isOriginAllowed(origin) {
			c.Next()
			return
		}
		header := c.Writer.Header()
		header.Add("Vary", "Origin")
		if config.AllowAllOrigins {
			header.Set("Access-Control-Allow-Origin", "*")
		} else {
			header.Set("Access-Control-Allow-Origin", origin)
		}
		if config.AllowCredentials {
			header.Set("Access-Control-Allow-Credentials", "true")
		}
		if len(config.ExposeHeaders) > 0 {
			header.Set("Access-Control-Expose-Headers", strings.Join(config.ExposeHeaders, ","))
		}
		// 预检请求: 响应头之外再补方法集/头部集/缓存时长, 204 短路。
		// 204 无正文, 必须立即发头: 懒发头语义下链尾无人写出时会被
		// net/http 兜底成 200, 丢失 204 语义。
		if c.Request.Method == http.MethodOptions && c.GetHeader(headerAccessControlRequestMethod) != "" {
			header.Set("Access-Control-Allow-Methods", strings.Join(config.AllowMethods, ","))
			header.Set("Access-Control-Allow-Headers", strings.Join(config.AllowHeaders, ","))
			if config.MaxAge > time.Duration(0) {
				header.Set("Access-Control-Max-Age", strconv.Itoa(int(config.MaxAge.Seconds())))
			}
			c.Status(http.StatusNoContent)
			c.Writer.WriteHeaderNow()
			c.Abort()
			return
		}
		c.Next()
	}
}

// isOriginAllowed 按 AllowOriginFunc > AllowAllOrigins 的优先级判定。
func (cfg *Config) isOriginAllowed(origin string) bool {
	if cfg.AllowOriginFunc != nil {
		return cfg.AllowOriginFunc(origin)
	}
	return cfg.AllowAllOrigins
}
