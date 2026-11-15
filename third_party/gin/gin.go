// Package gin 提供 HTTP 路由与请求上下文。
//
// 语义要点:
//   - Context 链推进: 引擎入口一次 Next 循环推完整条链, 仅 Abort 截断;
//   - ResponseWriter 懒发头: WriteHeader 只记录状态, 首次 Write 或
//     WriteHeaderNow 才发出, handler 在写出前仍可修改响应头;
//   - render 系(JSON/String/Data)只在 Content-Type 未设置时写入,
//     handler 预设的类型优先;
//   - 路径参数 :name 经 ServeMux 的 {name} 实现, Param(name) 同名取值。
package gin

import "net/http"

// H 是 JSON 渲染用的 map 别名。
type H map[string]any

// HandlerFunc 是处理链的单元。
type HandlerFunc func(*Context)

// 模式常量; 仅记录, 不影响行为。
const (
	DebugMode   = "debug"
	ReleaseMode = "release"
)

var mode = ReleaseMode

// SetMode 记录运行模式, 不影响行为。
func SetMode(m string) { mode = m }

// RouterGroup 持有一组共享前缀与中间件的路由。
type RouterGroup struct {
	basePath    string
	engine      *Engine
	middlewares []HandlerFunc
}

// Engine 是 HTTP 引擎, 实现了 http.Handler。
type Engine struct {
	RouterGroup
	mux        *http.ServeMux
	globals    []HandlerFunc // 全局中间件, 参与所有路由链与兜底链。
	notFound   HandlerFunc   // 未匹配路由的 404 兜底, 只属于兜底链。
	registered bool
}

// New 创建引擎, 不带任何中间件。
func New() *Engine {
	e := &Engine{
		mux: http.NewServeMux(),
	}
	e.engine = e
	e.basePath = "/"
	// 未匹配业务路由的请求(静态资源之外的杂散路径)落到兜底链:
	// 404 只挂在兜底链而非全局中间件: 混入全局会让它在业务链的
	// Auth 等写入者之前执行, 那时 Written 必然为 false, 会提前写 404。
	e.notFound = func(c *Context) {
		if !c.Writer.Written() {
			http.NotFound(c.Writer, c.Request)
		}
	}
	// 兜底路由注册两种 pattern:
	//   "/"        —— 一般未匹配请求;
	//   "OPTIONS /" —— 路径存在但方法不匹配的 OPTIONS(典型如 CORS 预检)。
	// ServeMux 对"路径命中而方法不匹配"会直接回 405 并绕过任何 handler,
	// 预检必须经过全局链里的 Cors 中间件才能拿到 204 与 CORS 头,
	// 因此 OPTIONS 需要自己的兜底入口。
	e.mux.Handle("OPTIONS /", e.fallbackChain())
	e.mux.Handle("/", e.fallbackChain())
	return e
}

// Use 追加全局中间件; 必须在注册任何路由之前调用。
func (e *Engine) Use(middlewares ...HandlerFunc) {
	if e.registered {
		panic("gin: Use must be called before routes are registered")
	}
	e.globals = append(e.globals, middlewares...)
}

// Group 创建共享前缀与中间件的子组。
func (g *RouterGroup) Group(relativePath string, middlewares ...HandlerFunc) *RouterGroup {
	return &RouterGroup{
		basePath:    joinPaths(g.basePath, relativePath),
		engine:      g.engine,
		middlewares: append(append([]HandlerFunc{}, g.middlewares...), middlewares...),
	}
}

// GET 注册 GET 路由。
func (g *RouterGroup) GET(path string, handlers ...HandlerFunc) {
	g.handle(http.MethodGet, path, handlers)
}

// POST 注册 POST 路由。
func (g *RouterGroup) POST(path string, handlers ...HandlerFunc) {
	g.handle(http.MethodPost, path, handlers)
}

// PUT 注册 PUT 路由。
func (g *RouterGroup) PUT(path string, handlers ...HandlerFunc) {
	g.handle(http.MethodPut, path, handlers)
}

// DELETE 注册 DELETE 路由。
func (g *RouterGroup) DELETE(path string, handlers ...HandlerFunc) {
	g.handle(http.MethodDelete, path, handlers)
}

// HEAD 注册 HEAD 路由。
func (g *RouterGroup) HEAD(path string, handlers ...HandlerFunc) {
	g.handle(http.MethodHead, path, handlers)
}

// OPTIONS 注册 OPTIONS 路由。
func (g *RouterGroup) OPTIONS(path string, handlers ...HandlerFunc) {
	g.handle(http.MethodOptions, path, handlers)
}

// PATCH 注册 PATCH 路由。
func (g *RouterGroup) PATCH(path string, handlers ...HandlerFunc) {
	g.handle(http.MethodPatch, path, handlers)
}

// handle 把链注册到 ServeMux: "METHOD path" pattern, :name 转为 {name}。
// ServeMux 精确 pattern 优先于兜底的 "/", 业务路由不会被兜底拦截;
// 未匹配的请求由 ServeMux 落到兜底链(全局中间件 + 404)。
func (g *RouterGroup) handle(method, relativePath string, handlers []HandlerFunc) {
	if len(handlers) == 0 {
		return
	}
	g.engine.registered = true
	// 链 = 全局中间件 + 组中间件 + 路由处理器:
	// r.Use 的全局中间件(Recovery/Cors/Static 等)必须作用于每条业务路由。
	chain := make([]HandlerFunc, 0, len(g.engine.globals)+len(g.middlewares)+len(handlers))
	chain = append(chain, g.engine.globals...)
	chain = append(chain, g.middlewares...)
	chain = append(chain, handlers...)
	pattern := method + " " + normalizePattern(joinPaths(g.basePath, relativePath))
	g.engine.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		c := acquireContext()
		c.reset(r, newResponseWriter(w), chain)
		defer releaseContext(c)
		c.Next()
	})
}

// ServeHTTP 实现了 http.Handler, 委托内部 ServeMux。
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mux.ServeHTTP(w, r)
}

// fallbackChain 组装兜底链: 全局中间件 + 404。
// 链必须在每次请求时现取: New 早于 Use, 若在注册期固化切片,
// 兜底链会永远停留在空中间件集合(静态资源与预检全部失效)。
func (e *Engine) fallbackChain() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chain := append(append([]HandlerFunc{}, e.globals...), e.notFound)
		c := &Context{
			Request:  r,
			Writer:   newResponseWriter(w),
			handlers: chain,
			index:    -1,
		}
		c.Next()
	})
}

// normalizePattern 把 :name 参数语法转成 ServeMux 的 {name}。
func normalizePattern(path string) string {
	out := make([]byte, 0, len(path))
	for i := 0; i < len(path); i++ {
		if path[i] == ':' {
			out = append(out, '{')
			j := i + 1
			for j < len(path) && path[j] != '/' {
				out = append(out, path[j])
				j++
			}
			out = append(out, '}')
			i = j - 1
			continue
		}
		out = append(out, path[i])
	}
	return string(out)
}

// joinPaths 拼接组前缀与相对路径。
func joinPaths(absolutePath, relativePath string) string {
	if relativePath == "" {
		return absolutePath
	}
	finalPath := join(absolutePath, relativePath)
	if len(relativePath) > 0 && relativePath[len(relativePath)-1] == '/' &&
		(len(finalPath) == 0 || finalPath[len(finalPath)-1] != '/') {
		return finalPath + "/"
	}
	return finalPath
}

// join 简化版 path.Join, 保留 :param 段(path.Join 会规范化为原样)。
func join(base, rel string) string {
	if base == "" {
		base = "/"
	}
	if !startsWith(base, "/") {
		base = "/" + base
	}
	if !startsWith(rel, "/") {
		rel = "/" + rel
	}
	if base == "/" {
		return rel
	}
	return base + rel
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
