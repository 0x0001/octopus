// Package sse 编码 Server-Sent Events。
//
// 线上格式:
//   - 字段序 id → event → retry → data, 每行 "key:value"(冒号后无空格);
//   - id/event 的值中换行转义为字面 "\n"、"\r";
//   - data: []byte/string 原样写出, struct/map/slice 经 json.Marshal 单行写出,
//     nil 写作 <nil>, 其余类型 fmt.Sprint;
//   - data 值中的换行替换为续行 "\ndata:";
//   - 事件以空行 "\n\n" 结束。
package sse

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// Event 是一条 SSE 事件。
type Event struct {
	Event string // event: 字段, 为空则省略该行。
	Id    string // id: 字段, 为空则省略。
	Retry uint   // retry: 字段, 0 则省略。
	Data  any    // data: 载荷, 见包注释的各类型编码规则。
}

var dataReplacer = strings.NewReplacer("\r\n", "\ndata:", "\r", "\ndata:", "\n", "\ndata:")

var fieldEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// Encode 向 w 写出一条完整的 SSE 事件(以空行结束)。
func Encode(w io.Writer, event Event) error {
	var buf []byte
	if len(event.Id) > 0 {
		buf = append(buf, "id:"...)
		buf = append(buf, fieldEscaper.Replace(event.Id)...)
		buf = append(buf, '\n')
	}
	if len(event.Event) > 0 {
		buf = append(buf, "event:"...)
		buf = append(buf, fieldEscaper.Replace(event.Event)...)
		buf = append(buf, '\n')
	}
	if event.Retry > 0 {
		buf = append(buf, "retry:"...)
		buf = append(buf, strconv.Itoa(int(event.Retry))...)
		buf = append(buf, '\n')
	}
	buf = append(buf, "data:"...)
	buf = append(buf, dataReplacer.Replace(formatData(event.Data))...)
	buf = append(buf, '\n', '\n')
	_, err := w.Write(buf)
	return err
}

// formatData 把 data 载荷编码为单行文本。
func formatData(data any) string {
	switch value := data.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(value)
	case string:
		return value
	}
	value := reflect.ValueOf(data)
	kind := value.Kind()
	if kind == reflect.Pointer {
		if value.IsNil() {
			return "<nil>"
		}
		value = value.Elem()
		kind = value.Kind()
	}
	switch kind {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		encoded, err := json.Marshal(data)
		if err != nil {
			return fmt.Sprint(data)
		}
		return string(encoded)
	default:
		return fmt.Sprint(data)
	}
}
