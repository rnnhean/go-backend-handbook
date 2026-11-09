package main

// 网关层练习: 生产项目通常在这里做鉴权、限流、协议转换。
// 为了让练习保持零外部依赖, 这个二进制只演示"网关独立启动"的结构。
import (
	"log"
	"net/http"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("gateway ok\n"))
	})
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	log.Println("gateway listening on :8080")
	log.Fatal(srv.ListenAndServe())
}
