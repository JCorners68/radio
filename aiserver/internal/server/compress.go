package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

type compressedResponseWriter struct {
	http.ResponseWriter
	writer      io.WriteCloser
	encoding    string
	wroteHeader bool
	compressed  bool
}

func (c *compressedResponseWriter) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	if code == http.StatusNoContent || code == http.StatusNotModified || (code >= 100 && code < 200) || (code >= 300 && code < 400) {
		c.ResponseWriter.WriteHeader(code)
		return
	}
	c.compressed = true
	c.Header().Del("Content-Length")
	c.Header().Set("Content-Encoding", c.encoding)
	c.Header().Add("Vary", "Accept-Encoding")
	c.ResponseWriter.WriteHeader(code)
}

func (c *compressedResponseWriter) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	if !c.compressed {
		return c.ResponseWriter.Write(b)
	}
	return c.writer.Write(b)
}

func (c *compressedResponseWriter) Flush() {
	if c.compressed {
		if f, ok := c.writer.(interface{ Flush() error }); ok {
			_ = f.Flush()
		}
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *compressedResponseWriter) Close() {
	if c.compressed && c.writer != nil {
		_ = c.writer.Close()
	}
}

// compressMiddleware wraps HTML, HTMX, JS, and JSON responses in Brotli (br) or Gzip
// compression, skipping binary audio streams (/api/stream/) and streaming NDJSON (/api/find).
func compressMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/stream/") || r.URL.Path == "/api/find" || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		ae := r.Header.Get("Accept-Encoding")
		var cw *compressedResponseWriter
		switch {
		case strings.Contains(ae, "br"):
			cw = &compressedResponseWriter{
				ResponseWriter: w,
				writer:         brotli.NewWriterLevel(w, 4),
				encoding:       "br",
			}
		case strings.Contains(ae, "gzip"):
			gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			cw = &compressedResponseWriter{
				ResponseWriter: w,
				writer:         gz,
				encoding:       "gzip",
			}
		default:
			next.ServeHTTP(w, r)
			return
		}
		defer cw.Close()
		next.ServeHTTP(cw, r)
	})
}
