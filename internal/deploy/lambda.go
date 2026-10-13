package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
)

// maxLambdaResponse is the largest response payload a synchronous Lambda
// invocation may return (6 MB), minus headroom for headers and JSON framing.
const maxLambdaResponse = 6*1000*1000 - 64*1024

// LambdaHandler adapts an http.Handler to AWS Lambda function URL (and API
// Gateway HTTP API v2) events, including base64 bodies.
//
// The request gets the event's method, raw path and query, headers, cookies
// and body (base64-decoded when flagged); RemoteAddr is the caller's source
// IP. Responses carry the status, headers (multi-valued ones comma-joined),
// Set-Cookie values as cookies, and a body that is base64-encoded unless it
// is valid UTF-8 text. Responses over Lambda's 6 MB limit become a JSON 413.
func LambdaHandler(h http.Handler) func(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	return func(ctx context.Context, ev events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
		r, err := lambdaToHTTP(ctx, ev)
		if err != nil {
			return lambdaJSONError(http.StatusBadRequest, CodeBadRequest, err.Error()), nil
		}
		w := &lambdaWriter{header: http.Header{}}
		h.ServeHTTP(w, r)
		return w.response(), nil
	}
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
}

func (w *lambdaWriter) Header() http.Header { return w.header }

func (w *lambdaWriter) WriteHeader(status int) {
	if w.wroteHeader || status < 200 {
		return // informational responses are not supported
	}
	w.status, w.wroteHeader = status, true
}

func (w *lambdaWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.body.Len()+len(p) > maxLambdaResponse+1 {
		// Keep counting past the limit without buffering everything.
		room := max(0, maxLambdaResponse+1-w.body.Len())
		w.body.Write(p[:room])
		return len(p), nil
	}
	return w.body.Write(p)
}

// Flush implements http.Flusher (a no-op: Lambda responses are buffered).
func (w *lambdaWriter) Flush() {}

func (w *lambdaWriter) response() events.LambdaFunctionURLResponse {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	body := w.body.Bytes()
	if len(body) > maxLambdaResponse {
		return lambdaJSONError(http.StatusRequestEntityTooLarge, CodeTooLarge,
			"response exceeds the AWS Lambda payload limit (6 MB); use the server's embedded gateway for large files")
	}
	if len(body) > 0 && w.header.Get("Content-Type") == "" {
		w.header.Set("Content-Type", http.DetectContentType(body))
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
	if isTextContent(w.header.Get("Content-Type")) && utf8.Valid(body) {
		resp.Body = string(body)
	} else if len(body) > 0 {
		resp.Body = base64.StdEncoding.EncodeToString(body)
		resp.IsBase64Encoded = true
	}
	return resp
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
