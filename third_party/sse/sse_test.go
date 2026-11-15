package sse

import (
	"bytes"
	"testing"
)

// 期望值全部取自 gin-contrib/sse(looplj fork, v1.1.2)对同样输入的逐字节输出,
// 客户端按标准 SSE 解析, 格式回归会直接破坏下游解析。
func TestEncodeMatchesReferenceFormat(t *testing.T) {
	type sample struct {
		name  string
		event Event
		want  string
	}
	samples := []sample{
		{
			name:  "[]byte",
			event: Event{Event: "message_start", Data: []byte(`{"type":"message_start","nested":"line1\nline2"}`)},
			want:  "event:message_start\ndata:{\"type\":\"message_start\",\"nested\":\"line1\\nline2\"}\n\n",
		},
		{
			name:  "string 含换行续行",
			event: Event{Data: "line1\nline2"},
			want:  "data:line1\ndata:line2\n\n",
		},
		{
			name:  "string",
			event: Event{Event: "delta", Data: "plain"},
			want:  "event:delta\ndata:plain\n\n",
		},
		{
			name: "struct",
			event: Event{Event: "log", Data: struct {
				ID string `json:"id"`
			}{ID: "a"}},
			want: "event:log\ndata:{\"id\":\"a\"}\n\n",
		},
		{
			name:  "map",
			event: Event{Data: map[string]any{"x": 1.5, "y": true}},
			want:  "data:{\"x\":1.5,\"y\":true}\n\n",
		},
		{
			name:  "nil data",
			event: Event{Event: "ping"},
			want:  "event:ping\ndata:<nil>\n\n",
		},
		{
			name:  "nil pointer",
			event: Event{Data: (*struct{})(nil)},
			want:  "data:<nil>\n\n",
		},
		{
			name:  "float",
			event: Event{Data: 1.5},
			want:  "data:1.5\n\n",
		},
		{
			name:  "int",
			event: Event{Data: 42},
			want:  "data:42\n\n",
		},
		{
			name:  "bool",
			event: Event{Data: true},
			want:  "data:true\n\n",
		},
		{
			name:  "event 值换行转义",
			event: Event{Event: "multi\nline", Data: "x"},
			want:  "event:multi\\nline\ndata:x\n\n",
		},
		{
			name:  "id 值换行转义",
			event: Event{Id: "id\nx", Data: "x"},
			want:  "id:id\\nx\ndata:x\n\n",
		},
		{
			name:  "retry",
			event: Event{Retry: 3000, Data: "x"},
			want:  "retry:3000\ndata:x\n\n",
		},
		{
			name:  "全字段按 id/event/retry/data 顺序",
			event: Event{Id: "9", Event: "delta", Retry: 100, Data: []byte(`{"a":1}`)},
			want:  "id:9\nevent:delta\nretry:100\ndata:{\"a\":1}\n\n",
		},
	}
	for _, s := range samples {
		t.Run(s.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Encode(&buf, s.event); err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if buf.String() != s.want {
				t.Fatalf("got %q, want %q", buf.String(), s.want)
			}
		})
	}
}
