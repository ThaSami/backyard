package deploy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
)

const (
	// lambdaPayloadLimit bounds the response payload of a synchronous Lambda
	// invocation: the JSON document the runtime returns (status, headers and
	// the body, base64-encoded unless it is text). AWS allows 6 MiB; this
	// keeps a margin for the runtime's own framing.
	lambdaPayloadLimit = 6 * 1000 * 1000
	// lambdaBufferLimit bounds how much of a compressible response body the
	// adapter buffers: large JSON (the presigned upload URLs of a deploy of
	// thousands of files) still fits the payload once gzipped.
	lambdaBufferLimit = 64 << 20
	// lambdaGzipMin is the smallest body worth compressing.
	lambdaGzipMin = 32 << 10
)

// LambdaHandler adapts an http.Handler to AWS Lambda function URL (and API
// Gateway HTTP API v2) events, including base64 bodies.
//
// The request gets the event's method, raw path and query, headers, cookies
// and body (base64-decoded when flagged); RemoteAddr is the caller's source
// IP. Responses carry the status, headers (multi-valued ones comma-joined),
// Set-Cookie values as cookies, and a body that is base64-encoded unless it
// is valid UTF-8 text. A text body of at least 32 KiB is gzipped when the
// request accepts it (Go clients do by default), so that e.g. the upload
// list of a deploy of thousands of files fits Lambda's 6 MB response
// payload. A response whose payload would still exceed that limit
// (measured after base64 and JSON encoding) becomes a JSON 413 that
// points to the server's embedded gateway, which has no such limit.
func LambdaHandler(h http.Handler) func(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	advice := "use the server's embedded gateway (<public_url>" + EmbeddedPath + ") instead"
	if g, ok := h.(*Gateway); ok && g.cfg.Server.PublicURL != "" {
		pub := g.cfg.Server.PublicURL
		advice = "use the server's embedded gateway instead: backyard login --server " + pub + " --gateway " + pub + EmbeddedPath
	}
	return func(ctx context.Context, ev events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
		r, err := lambdaToHTTP(ctx, ev)
		if err != nil {
			return lambdaJSONError(http.StatusBadRequest, CodeBadRequest, err.Error()), nil
		}
		w := &lambdaWriter{header: http.Header{}, gzip: acceptsGzip(r.Header.Values("Accept-Encoding"))}
		h.ServeHTTP(w, r)
		return w.response(advice), nil
	}
}

// acceptsGzip reports whether Accept-Encoding values allow a gzip body.
func acceptsGzip(values []string) bool {
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			name, params, _ := strings.Cut(part, ";")
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "gzip" && name != "x-gzip" {
				continue
			}
			q := 1.0
			for p := range strings.SplitSeq(params, ";") {
				if k, val, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
						q = f
					}
				}
			}
			if q > 0 {
				return true
			}
		}
	}
	return false
}

// lambdaToHTTP converts a function URL event to an *http.Request.
func lambdaToHTTP(ctx context.Context, ev events.LambdaFunctionURLRequest) (*http.Request, error) {
	method := ev.RequestContext.HTTP.Method
	if method == "" {
		method = http.MethodGet
	}
	rawPath := ev.RawPath
	if rawPath == "" {
		rawPath = ev.RequestContext.HTTP.Path
	}
	if rawPath == "" || rawPath[0] != '/' {
		rawPath = "/" + rawPath
	}
	pu, err := url.Parse(rawPath)
	if err != nil {
		return nil, fmt.Errorf("invalid request path: %w", err)
	}

	var body []byte
	if ev.IsBase64Encoded {
		body, err = base64.StdEncoding.DecodeString(ev.Body)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 body: %w", err)
		}
	} else {
		body = []byte(ev.Body)
	}

	host := ""
	for k, v := range ev.Headers {
		if strings.EqualFold(k, "host") {
			host = v
		}
	}
	if host == "" {
		host = ev.RequestContext.DomainName
	}
	if host == "" {
		host = "lambda.invalid"
	}

	u := &url.URL{Scheme: "https", Host: host, Path: pu.Path, RawPath: pu.RawPath, RawQuery: ev.RawQueryString}
	r, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	r.RequestURI = u.RequestURI()
	r.Host = host
	r.ContentLength = int64(len(body))
	if len(body) == 0 {
		r.Body = http.NoBody
	}
	for k, v := range ev.Headers {
		if strings.EqualFold(k, "content-length") || strings.EqualFold(k, "host") {
			continue
		}
		r.Header.Set(k, v)
	}
	if len(ev.Cookies) > 0 {
		r.Header.Set("Cookie", strings.Join(ev.Cookies, "; "))
	}
	if r.Header.Get("X-Forwarded-Proto") == "" {
		r.Header.Set("X-Forwarded-Proto", "https") // function URLs are HTTPS-only
	}
	if ip := ev.RequestContext.HTTP.SourceIP; ip != "" {
		r.RemoteAddr = net.JoinHostPort(ip, "0")
	}
	return r, nil
}

// lambdaWriter buffers a response for conversion to a Lambda result.
type lambdaWriter struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
	gzip        bool // the client accepts a gzip-encoded body
	limit       int  // body bytes to buffer, set by WriteHeader
	overflow    int64
}

func (w *lambdaWriter) Header() http.Header { return w.header }

func (w *lambdaWriter) WriteHeader(status int) {
	if w.wroteHeader || status < 200 {
		return // informational responses are not supported
	}
	w.status, w.wroteHeader = status, true
	// A body that is not compressed only grows on its way into the payload
	// (base64, JSON escaping): past the payload limit it cannot fit.
	w.limit = lambdaPayloadLimit
	if w.compressible() {
		w.limit = lambdaBufferLimit
	}
}

// compressible reports whether the response may be gzipped.
func (w *lambdaWriter) compressible() bool {
	return w.gzip && w.header.Get("Content-Encoding") == "" && isTextContent(w.header.Get("Content-Type"))
}

func (w *lambdaWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.overflow > 0 || w.body.Len()+len(p) > w.limit {
		// Too large to return: keep counting without buffering.
		if w.overflow == 0 {
			w.overflow = int64(w.body.Len())
			w.body = bytes.Buffer{}
		}
		w.overflow += int64(len(p))
		return len(p), nil
	}
	return w.body.Write(p)
}

// Flush implements http.Flusher (a no-op: Lambda responses are buffered).
func (w *lambdaWriter) Flush() {}

func (w *lambdaWriter) response(advice string) events.LambdaFunctionURLResponse {
	tooLarge := func(size string) events.LambdaFunctionURLResponse {
		return lambdaJSONError(http.StatusRequestEntityTooLarge, CodeTooLarge, fmt.Sprintf(
			"the response (%s) is larger than an AWS Lambda function can return (6 MB, after encoding); %s", size, advice))
	}
	if w.overflow > 0 {
		return tooLarge(fmt.Sprintf("%d bytes", w.overflow))
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	body := w.body.Bytes()
	if len(body) > 0 && w.header.Get("Content-Type") == "" {
		w.header.Set("Content-Type", http.DetectContentType(body))
	}
	binary := !isTextContent(w.header.Get("Content-Type")) || !utf8.Valid(body)
	if len(body) >= lambdaGzipMin && w.compressible() {
		if gz, err := gzipBytes(body); err == nil && len(gz) < len(body) {
			body, binary = gz, true
			w.header.Set("Content-Encoding", "gzip")
			w.header.Add("Vary", "Accept-Encoding")
			// The encoded representation is not byte-identical to the
			// stored object any more.
			if et := w.header.Get("ETag"); et != "" && !strings.HasPrefix(et, "W/") {
				w.header.Set("ETag", "W/"+et)
			}
		}
	}
	resp := events.LambdaFunctionURLResponse{StatusCode: status, Headers: map[string]string{}}
	for k, vs := range w.header {
		switch http.CanonicalHeaderKey(k) {
		case "Set-Cookie":
			resp.Cookies = append(resp.Cookies, vs...)
			continue
		case "Content-Length", "Transfer-Encoding", "Connection", "Keep-Alive", "Trailer", "Upgrade":
			// Lambda frames the payload itself; a handler's length (e.g.
			// for a HEAD response, whose body is empty) must not leak.
			continue
		}
		resp.Headers[k] = strings.Join(vs, ",")
	}
	if !binary {
		resp.Body = string(body)
	} else if len(body) > 0 {
		resp.Body = base64.StdEncoding.EncodeToString(body)
		resp.IsBase64Encoded = true
	}
	if n := lambdaPayloadSize(resp); n > lambdaPayloadLimit {
		return tooLarge(fmt.Sprintf("%d bytes as a Lambda payload", n))
	}
	return resp
}

// gzipBytes compresses b.
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// lambdaPayloadSize returns the size of resp as the Lambda runtime encodes
// it (aws-lambda-go: a JSON encoder without HTML escaping).
func lambdaPayloadSize(resp events.LambdaFunctionURLResponse) int64 {
	var n countingWriter
	if err := jsonEncode(&n, resp); err != nil {
		return math.MaxInt64
	}
	return int64(n)
}

// countingWriter counts the bytes written to it.
type countingWriter int64

func (c *countingWriter) Write(p []byte) (int, error) {
	*c += countingWriter(len(p))
	return len(p), nil
}

// isTextContent reports whether a content type is safe to return as text.
func isTextContent(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "text/"):
		return true
	case strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	switch mt {
	case "application/json", "application/javascript", "application/xml",
		"application/x-www-form-urlencoded", "image/svg+xml", "application/x-ndjson":
		return true
	}
	return false
}

func lambdaJSONError(status int, code, msg string) events.LambdaFunctionURLResponse {
	var eb ErrorBody
	eb.Error.Code, eb.Error.Message = code, msg
	var buf bytes.Buffer
	_ = jsonEncode(&buf, eb)
	return events.LambdaFunctionURLResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       buf.String(),
	}
}
