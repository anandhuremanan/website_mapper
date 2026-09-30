package api

import (
	"compress/gzip"
	"net/http"
	"sync"
)

// gzipPool reuses compressors. BestSpeed keeps CPU low: result JSON is
// highly repetitive (hostnames, paths, field names) and still shrinks
// several-fold, which saves outbound bandwidth to browsers.
var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return w
}}

// gzipWriter compresses a response for clients that accept gzip.
type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func newGzipWriter(w http.ResponseWriter) *gzipWriter {
	gz := gzipPool.Get().(*gzip.Writer)
	gz.Reset(w)
	return &gzipWriter{ResponseWriter: w, gz: gz}
}

func (g *gzipWriter) WriteHeader(code int) {
	if !g.wroteHeader {
		g.wroteHeader = true
		h := g.Header()
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		h.Del("Content-Length")
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	return g.gz.Write(p)
}

// Close flushes the compressed stream and returns the compressor to the pool.
func (g *gzipWriter) Close() {
	if g.wroteHeader {
		g.gz.Close()
	}
	gzipPool.Put(g.gz)
}
