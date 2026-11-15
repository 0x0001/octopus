package rhttp

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDirectClientNegotiatesCompression 透传(client.Do)与 pipeline(DoStream)
// 共用 rhttp 返回的客户端, 在 Transport 层接入压缩协商后两条上游路径全覆盖:
// 上游按协商压缩, 客户端拿到的正文必须透明解压, 压缩头同步剥除。
func TestDirectClientNegotiatesCompression(t *testing.T) {
	const payload = "data: upstream sse frame\n\n"

	requestSeen := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gw := gzip.NewWriter(w)
		if _, err := gw.Write([]byte(payload)); err != nil {
			t.Errorf("write: %v", err)
		}
		if err := gw.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}))
	defer server.Close()

	client, err := Direct()
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()

	if requestSeen != "gzip, zstd, deflate, br" {
		t.Fatalf("Accept-Encoding = %q, want %q", requestSeen, "gzip, zstd, deflate, br")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != payload {
		t.Fatalf("body = %q, %v; want %q", body, err, payload)
	}
	if response.Header.Get("Content-Encoding") != "" {
		t.Error("Content-Encoding not stripped")
	}
}
