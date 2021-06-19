package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/go-kratos/kratos/v2/encoding"
	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/internal/httputil"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/go-kratos/kratos/v2/transport/http/balancer"
	"github.com/go-kratos/kratos/v2/transport/http/balancer/random"
)

// Why 这样子设计？？？

// DecodeErrorFunc is decode error func.
// 客户端-响应解码器
type DecodeErrorFunc func(ctx context.Context, res *http.Response) error

// EncodeRequestFunc is request encode func.
// 客户端-请求编码器
type EncodeRequestFunc func(ctx context.Context, contentType string, in interface{}) (body []byte, err error)

// DecodeResponseFunc is response decode func.
// 客户端-响应解码器，带有输出对象
type DecodeResponseFunc func(ctx context.Context, res *http.Response, out interface{}) error

// ClientOption is HTTP client option.
type ClientOption func(*clientOptions)

// Why clientOptions是小写开头，而ServerOptions是大写开头
// 是因为clientOptions没有被外面对象引用？正常也应该是小写，可以通过OptionFunc来设置
// Client is an HTTP transport client.
type clientOptions struct {
	ctx          context.Context
	timeout      time.Duration           // 超时时间？ (connect, write, read)
	endpoint     string                  // 请求服务器的地址
	userAgent    string                  // 设置用户请求-Agent
	encoder      EncodeRequestFunc       // 请求编码器
	decoder      DecodeResponseFunc      // 响应解码器
	errorDecoder DecodeErrorFunc         // 响应解码器
	transport    http.RoundTripper       // tcp传输层
	balancer     balancer.Balancer       // 请求负载均衡
	discovery    registry.Discovery      // 服务发现
	middleware   []middleware.Middleware // 中间件
}

// WithTransport with client transport.
func WithTransport(trans http.RoundTripper) ClientOption {
	return func(o *clientOptions) {
		o.transport = trans
	}
}

// WithTimeout with client request timeout.
func WithTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) {
		o.timeout = d
	}
}

// WithUserAgent with client user agent.
func WithUserAgent(ua string) ClientOption {
	return func(o *clientOptions) {
		o.userAgent = ua
	}
}

// WithMiddleware with client middleware.
func WithMiddleware(m ...middleware.Middleware) ClientOption {
	return func(o *clientOptions) {
		o.middleware = m
	}
}

// WithEndpoint with client addr.
func WithEndpoint(endpoint string) ClientOption {
	return func(o *clientOptions) {
		o.endpoint = endpoint
	}
}

// WithRequestEncoder with client request encoder.
func WithRequestEncoder(encoder EncodeRequestFunc) ClientOption {
	return func(o *clientOptions) {
		o.encoder = encoder
	}
}

// WithResponseDecoder with client response decoder.
func WithResponseDecoder(decoder DecodeResponseFunc) ClientOption {
	return func(o *clientOptions) {
		o.decoder = decoder
	}
}

// WithErrorDecoder with client error decoder.
func WithErrorDecoder(errorDecoder DecodeErrorFunc) ClientOption {
	return func(o *clientOptions) {
		o.errorDecoder = errorDecoder
	}
}

// WithDiscovery with client discovery.
func WithDiscovery(d registry.Discovery) ClientOption {
	return func(o *clientOptions) {
		o.discovery = d
	}
}

// WithBalancer with client balancer.
// Experimental
// Notice: This type is EXPERIMENTAL and may be changed or removed in a later release.
func WithBalancer(b balancer.Balancer) ClientOption {
	return func(o *clientOptions) {
		o.balancer = b
	}
}

// Client is an HTTP client.
// 封装http client的客户端对象
type Client struct {
	opts   clientOptions
	target *Target   // 访问服务实例
	r      *resolver // 服务地址-解析器
	cc     *http.Client
}

// NewClient returns an HTTP client.
func NewClient(ctx context.Context, opts ...ClientOption) (*Client, error) {
	options := clientOptions{
		ctx:          ctx,
		timeout:      500 * time.Millisecond, // 注意，默认超时为500ms, 说明微服务接口响应要快，避免服务出现雪崩
		encoder:      DefaultRequestEncoder,
		decoder:      DefaultResponseDecoder,
		errorDecoder: DefaultErrorDecoder,
		transport:    http.DefaultTransport,
		balancer:     random.New(), // 随机均衡
	}
	for _, o := range opts {
		o(&options)
	}
	target, err := parseTarget(options.endpoint)
	if err != nil {
		return nil, err
	}
	var r *resolver
	// 有用到服务发现
	if options.discovery != nil {
		// 发现协议
		if target.Scheme == "discovery" {
			// 服务-解析器
			if r, err = newResolver(ctx, options.discovery, target); err != nil {
				return nil, fmt.Errorf("[http client] new resolver failed!err: %v", options.endpoint)
			}
		} else {
			return nil, fmt.Errorf("[http client] invalid endpoint format: %v", options.endpoint)
		}
	}
	return &Client{
		opts:   options,
		target: target,
		r:      r,
		cc: &http.Client{
			Timeout:   options.timeout,
			Transport: options.transport,
		},
	}, nil
}

// Invoke makes an rpc call procedure for remote service.
// 发起rpc请求
// method 是http method
func (client *Client) Invoke(ctx context.Context, method, path string, args interface{}, reply interface{}, opts ...CallOption) error {
	var (
		contentType string
		body        io.Reader
	)
	c := defaultCallInfo(path)
	for _, o := range opts {
		// call请求发起之前，保存一些信息到CallInfo
		if err := o.before(&c); err != nil {
			return err
		}
	}
	if args != nil {
		data, err := client.opts.encoder(ctx, c.contentType, args)
		if err != nil {
			return err
		}
		// 保存contentType 解码用到
		contentType = c.contentType
		body = bytes.NewReader(data)
	}
	// 构造http请求
	url := fmt.Sprintf("%s://%s%s", client.target.Scheme, client.target.Authority, path)
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}
	if client.opts.userAgent != "" {
		req.Header.Set("User-Agent", client.opts.userAgent)
	}
	// 客户端请求上下文, 包含Transport接口信息
	ctx = transport.NewClientContext(ctx, &Transport{
		endpoint:     client.opts.endpoint,
		header:       headerCarrier(req.Header),
		operation:    c.operation,
		request:      req,
		pathTemplate: c.pathTemplate,
	})
	return client.invoke(ctx, req, args, reply, c)
}

func (client *Client) invoke(ctx context.Context, req *http.Request, args interface{}, reply interface{}, c callInfo) error {
	h := func(ctx context.Context, in interface{}) (interface{}, error) {
		var done func(context.Context, balancer.DoneInfo)
		// 服务解析器
		if client.r != nil {
			var (
				err  error
				node *registry.ServiceInstance
				// 根据客户端target, 获取服务节点列表
				nodes = client.r.fetch(ctx)
			)
			// 根据负载均衡策略，返回服务节点列表的中一个
			if node, done, err = client.opts.balancer.Pick(ctx, nodes); err != nil {
				return nil, errors.ServiceUnavailable("NODE_NOT_FOUND", err.Error())
			}
			scheme, addr, err := parseEndpoint(node.Endpoints)
			if err != nil {
				return nil, errors.ServiceUnavailable("NODE_NOT_FOUND", err.Error())
			}
			req = req.Clone(ctx)
			req.URL.Scheme = scheme
			req.URL.Host = addr
		}
		res, err := client.do(ctx, req, c)
		// 有完成处理函数，就调用它
		if done != nil {
			done(ctx, balancer.DoneInfo{Err: err})
		}
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		// 编码请求结果
		if err := client.opts.decoder(ctx, res, reply); err != nil {
			return nil, err
		}
		return reply, nil
	}
	// 如果有中间件，进行处理
	if len(client.opts.middleware) > 0 {
		h = middleware.Chain(client.opts.middleware...)(h)
	}
	_, err := h(ctx, args)
	return err
}

// Do send an HTTP request and decodes the body of response into target.
// returns an error (of type *Error) if the response status code is not 2xx.
func (client *Client) Do(req *http.Request, opts ...CallOption) (*http.Response, error) {
	c := defaultCallInfo(req.URL.Path)
	for _, o := range opts {
		if err := o.before(&c); err != nil {
			return nil, err
		}
	}
	return client.do(req.Context(), req, c)
}

func (client *Client) do(ctx context.Context, req *http.Request, c callInfo) (*http.Response, error) {
	resp, err := client.cc.Do(req)
	if err != nil {
		return nil, err
	}
	if err := client.opts.errorDecoder(ctx, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// DefaultRequestEncoder is an HTTP request encoder.
func DefaultRequestEncoder(ctx context.Context, contentType string, in interface{}) ([]byte, error) {
	name := httputil.ContentSubtype(contentType)
	body, err := encoding.GetCodec(name).Marshal(in)
	if err != nil {
		return nil, err
	}
	return body, err
}

// DefaultResponseDecoder is an HTTP response decoder.
func DefaultResponseDecoder(ctx context.Context, res *http.Response, v interface{}) error {
	defer res.Body.Close()
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}
	return CodecForResponse(res).Unmarshal(data, v)
}

// DefaultErrorDecoder is an HTTP error decoder.
func DefaultErrorDecoder(ctx context.Context, res *http.Response) error {
	// 正常
	if res.StatusCode >= 200 && res.StatusCode <= 299 {
		return nil
	}
	defer res.Body.Close()
	data, err := ioutil.ReadAll(res.Body)
	if err == nil {
		e := new(errors.Error)
		if err = CodecForResponse(res).Unmarshal(data, e); err == nil {
			e.Code = int32(res.StatusCode)
			return e
		}
	}
	return errors.Errorf(res.StatusCode, errors.UnknownReason, err.Error())
}

// CodecForResponse get encoding.Codec via http.Response
func CodecForResponse(r *http.Response) encoding.Codec {
	codec := encoding.GetCodec(httputil.ContentSubtype(r.Header.Get("Content-Type")))
	if codec != nil {
		return codec
	}
	return encoding.GetCodec("json")
}
