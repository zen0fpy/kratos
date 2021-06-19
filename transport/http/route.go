package http

import (
	"net/http"
	"path"
	"sync"
)

// FilterFunc is a function which receives an http.Handler and returns another http.Handler.
type FilterFunc func(http.Handler) http.Handler

// FilterChain returns a FilterFunc that specifies the chained handler for HTTP Router.
// http.Handler经过http路由过滤器链从后面开始过滤, 并返回处理过新的http.Handler
func FilterChain(filters ...FilterFunc) FilterFunc {
	return func(next http.Handler) http.Handler {
		for i := len(filters) - 1; i >= 0; i-- {
			next = filters[i](next)
		}
		return next
	}
}

// Route is an HTTP route.
// 路由对象，包括有
// 路径前缀，所属服务器，过滤器
// 对象池
type Route struct {
	prefix  string
	pool    sync.Pool
	srv     *Server
	filters []FilterFunc
}

func newRoute(prefix string, srv *Server, filters ...FilterFunc) *Route {
	r := &Route{
		prefix:  prefix,
		srv:     srv,
		filters: filters,
	}
	r.pool.New = func() interface{} {
		return &wrapper{route: r}
	}
	return r
}

// Handle registers a new route with a matcher for the URL path and method.
// Handle 根据请求路径和方法来注册路由匹配器
func (r *Route) Handle(method, relativePath string, h HandlerFunc, filters ...FilterFunc) {
	// 先生成一个http.Handler
	next := http.Handler(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		ctx := r.pool.Get().(Context)
		// 设置请求上下文中的请求和响应数据
		ctx.Reset(res, req)
		// 处理请求
		if err := h(ctx); err != nil {
			r.srv.ene(res, req, err)
		}
		// 清空请求上下文请求和响应数据
		// 并放回对象池
		ctx.Reset(nil, nil)
		r.pool.Put(ctx)
	}))
	// 经过过滤链处理
	next = FilterChain(filters...)(next)
	next = FilterChain(r.filters...)(next)
	// 在调用http.Handler
	r.srv.router.Handle(path.Join(r.prefix, relativePath), next).Methods(method)
}

// GET registers a new GET route for a path with matching handler in the router.
func (r *Route) GET(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodGet, path, h, m...)
}

// HEAD registers a new HEAD route for a path with matching handler in the router.
func (r *Route) HEAD(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodHead, path, h, m...)
}

// POST registers a new POST route for a path with matching handler in the router.
func (r *Route) POST(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodPost, path, h, m...)
}

// PUT registers a new PUT route for a path with matching handler in the router.
func (r *Route) PUT(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodPut, path, h, m...)
}

// PATCH registers a new PATCH route for a path with matching handler in the router.
func (r *Route) PATCH(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodPatch, path, h, m...)
}

// DELETE registers a new DELETE route for a path with matching handler in the router.
func (r *Route) DELETE(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodDelete, path, h, m...)
}

// CONNECT registers a new CONNECT route for a path with matching handler in the router.
func (r *Route) CONNECT(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodConnect, path, h, m...)
}

// OPTIONS registers a new OPTIONS route for a path with matching handler in the router.
func (r *Route) OPTIONS(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodOptions, path, h, m...)
}

// TRACE registers a new TRACE route for a path with matching handler in the router.
func (r *Route) TRACE(path string, h HandlerFunc, m ...FilterFunc) {
	r.Handle(http.MethodTrace, path, h, m...)
}
