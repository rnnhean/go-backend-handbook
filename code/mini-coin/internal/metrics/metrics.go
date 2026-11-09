package metrics

import (
	"fmt"
	"net/http"
	"sync"
)

// Registry 是一个极简 Prometheus 文本格式实现, 用来理解指标模型。
// 真实项目直接用 prometheus/client_golang。
type Registry struct {
	mu       sync.Mutex
	counters map[string]float64
}

func New() *Registry { return &Registry{counters: make(map[string]float64)} }

func (r *Registry) Inc(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name]++
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		for name, value := range r.counters {
			_, _ = fmt.Fprintf(w, "# TYPE %s counter\n%s %g\n", name, name, value)
		}
	})
}
