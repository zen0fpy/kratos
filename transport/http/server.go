package http

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	ic "github.com/go-kratos/kratos/v2/internal/context"
	"github.com/go-kratos/kratos/v2/internal/host"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"

	"github.com/gorilla/mux"
)

// 这里是检验Server有没有实现TransportServer和Endpoint er接口
var _ transport.Server = (*Server)(nil)
var _ transport.Endpointer = (*Server)(nil)

// ServerOption is an HTTP server option.
type ServerOption func(*Server)

// Network with server network.
func Network(network string) ServerOption {
	return func(s *Server) {
		s.network = network
	}
}

// Address with server address.
func Address(addr string) ServerOption {
	return func(s *Server) {
		s.address = addr
	}
}

// Timeout with server timeout.
func Timeout(timeout time.Duration) ServerOption {
	return func(s *Server) {
		s.timeout = timeout
	}
}

// Logger with server logger.
func Logger(logger log.Logger) ServerOption {
	return func(s *Server) {
		s.log = log.NewHelper(logger)
	}
}

// Middleware with service middleware option.
func Middleware(m ...middleware.Middleware) ServerOption {
	return func(o *Server) {
		o.ms = m
	}
}

// Filter with HTTP middleware option.
func Filter(filters ...FilterFunc) ServerOption {
	return func(o *Server) {
		o.filters = filters
	}
}

// RequestDecoder with request decoder.
func RequestDecoder(dec DecodeRequestFunc) ServerOption {
	return func(o *Server) {
		o.dec = dec
	}
}

// ResponseEncoder with response encoder.
func ResponseEncoder(en EncodeResponseFunc) ServerOption {
	return func(o *Server) {
		o.enc = en
	}
}

// ErrorEncoder with error encoder.
// 为啥只有编码错误处理函数，而没有解码错误处理函数, 如果解码失败会怎么处理
func ErrorEncoder(en EncodeErrorFunc) ServerOption {
	return func(o *Server) {
		o.ene = en
	}
}

// Server is an HTTP server wrapper.
type Server struct {
	*http.Server
	ctx      context.Context
	lis      net.Listener
	once     sync.Once
	endpoint *url.URL
	err      error
	network  string
	address  string
	timeout  time.Duration
	filters  []FilterFunc
	ms       []middleware.Middleware
	dec      DecodeRequestFunc
	enc      EncodeResponseFunc
	ene      EncodeErrorFunc
	router   *mux.Router
	log      *log.Helper
}

// NewServer creates an HTTP server by options.
func NewServer(opts ...ServerOption) *Server {
	srv := &Server{
		network: "tcp",
		address: ":0",
		timeout: 1 * time.Second,
		dec:     DefaultRequestDecoder,
		enc:     DefaultResponseEncoder,
		ene:     DefaultErrorEncoder,
		log:     log.NewHelper(log.DefaultLogger),
	}
	for _, o := range opts {
		o(srv)
	}
	srv.Server = &http.Server{Handler: srv}
	srv.router = mux.NewRouter()
	// 添加过滤器
	srv.router.Use(srv.filter())
	return srv
}

// Route registers an HTTP route.
func (s *Server) Route(prefix string, filters ...FilterFunc) *Route {
	return newRoute(prefix, s, filters...)
}

// Handle registers a new route with a matcher for the URL path.
func (s *Server) Handle(path string, h http.Handler) {
	s.router.Handle(path, h)
}

// HandlePrefix registers a new route with a matcher for the URL path prefix.
func (s *Server) HandlePrefix(prefix string, h http.Handler) {
	s.router.PathPrefix(prefix).Handler(h)
}

// HandleFunc registers a new route with a matcher for the URL path.
func (s *Server) HandleFunc(path string, h http.HandlerFunc) {
	s.router.HandleFunc(path, h)
}

// ServeHTTP should write reply headers and data to the ResponseWriter and then return.
func (s *Server) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	s.router.ServeHTTP(res, req)
}

// 请求过滤处理函数
func (s *Server) filter() mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		// 左边next是经过路由器过滤链处理后新的http.Handler
		next = FilterChain(s.filters...)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// 合并上下文, 超时取决最快那个
			ctx, cancel := ic.Merge(req.Context(), s.ctx)
			defer cancel()

			// 如果server有设置超时，就用server的
			if s.timeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, s.timeout)
				defer cancel()
			}
			pathTemplate := req.URL.Path
			if route := mux.CurrentRoute(req); route != nil {
				// /path/123 -> /path/{id}
				pathTemplate, _ = route.GetPathTemplate()
			}
			// 构造传输层
			tr := &Transport{
				endpoint:     s.endpoint.String(),
				operation:    pathTemplate,
				header:       headerCarrier(req.Header),
				request:      req,
				pathTemplate: pathTemplate,
			}
			// 为啥还要设置一次?
			if r := mux.CurrentRoute(req); r != nil {
				if path, err := r.GetPathTemplate(); err == nil {
					tr.operation = path
				}
			}
			// 给http请求上下文，带上传输层基本信息，到后面会用到
			ctx = transport.NewServerContext(ctx, tr)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}

// Endpoint return a real address to registry endpoint.
// examples:
//   http://127.0.0.1:8000?isSecure=false
// 只是在启动时候执行，而且只执行一次
func (s *Server) Endpoint() (*url.URL, error) {
	s.once.Do(func() {
		lis, err := net.Listen(s.network, s.address)
		if err != nil {
			s.err = err
			return
		}
		// 前面已经监听address， 为啥这里还要获取addr
		// address 和 addr 有啥不同的呢？
		// addr是私有地址和端口, 内网通信
		addr, err := host.Extract(s.address, lis)
		if err != nil {
			lis.Close()
			s.err = err
			return
		}
		s.lis = lis
		s.endpoint = &url.URL{Scheme: "http", Host: addr}
	})
	if s.err != nil {
		return nil, s.err
	}
	return s.endpoint, nil
}

// Start start the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	// 监听失败就返回
	if _, err := s.Endpoint(); err != nil {
		return err
	}
	// 什么时候注册到注册中心
	s.ctx = ctx
	s.log.Infof("[HTTP] server listening on: %s", s.lis.Addr().String())
	if err := s.Serve(s.lis); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop stop the HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	s.log.Info("[HTTP] server stopping")
	// 这里不用传参ctx, 而是新建一个, 是怕还没有关闭，就超时退出?
	return s.Shutdown(context.Background())
}
