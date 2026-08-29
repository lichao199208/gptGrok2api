package provider

import (
	"io"
	stdhttp "net/http"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// browserHTTP adapts tls-client's fhttp request/response types to the
// standard library types used by the provider package.
type browserHTTP struct {
	client tlsclient.HttpClient
}

func newBrowserHTTP(proxyURL string, timeout time.Duration) (*browserHTTP, error) {
	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 30
	}
	options := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(profiles.Chrome_110),
		tlsclient.WithTimeoutSeconds(seconds),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
	}
	if proxyURL != "" {
		options = append(options, tlsclient.WithProxyUrl(proxyURL))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	return &browserHTTP{client: client}, nil
}

func (c *browserHTTP) Do(req *stdhttp.Request) (*stdhttp.Response, error) {
	converted, err := fhttp.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), req.Body)
	if err != nil {
		return nil, err
	}
	converted.Header = cloneFHTTPHeader(req.Header)
	converted.ContentLength = req.ContentLength
	converted.Close = req.Close
	converted.Host = req.Host

	response, err := c.client.Do(converted)
	if err != nil {
		return nil, err
	}
	return cloneStdHTTPResponse(response), nil
}

func (c *browserHTTP) CloseIdleConnections() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}

func cloneFHTTPHeader(source stdhttp.Header) fhttp.Header {
	target := make(fhttp.Header, len(source)+1)
	order := make([]string, 0, len(source))
	for key, values := range source {
		target[key] = append([]string(nil), values...)
		if key != "Cookie" && key != "Host" {
			order = append(order, key)
		}
	}
	if len(order) > 0 {
		target[fhttp.HeaderOrderKey] = order
	}
	return target
}

func cloneStdHTTPResponse(source *fhttp.Response) *stdhttp.Response {
	target := &stdhttp.Response{
		Status:           source.Status,
		StatusCode:       source.StatusCode,
		Proto:            source.Proto,
		ProtoMajor:       source.ProtoMajor,
		ProtoMinor:       source.ProtoMinor,
		Header:           cloneStdHeader(source.Header),
		Body:             source.Body,
		ContentLength:    source.ContentLength,
		TransferEncoding: append([]string(nil), source.TransferEncoding...),
		Close:            source.Close,
		Uncompressed:     source.Uncompressed,
		Trailer:          cloneStdHeader(source.Trailer),
	}
	if target.Body == nil {
		target.Body = io.NopCloser(nilReader{})
	}
	return target
}

func cloneStdHeader(source fhttp.Header) stdhttp.Header {
	target := make(stdhttp.Header, len(source))
	for key, values := range source {
		if key == fhttp.HeaderOrderKey || key == fhttp.PHeaderOrderKey {
			continue
		}
		target[key] = append([]string(nil), values...)
	}
	return target
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }
