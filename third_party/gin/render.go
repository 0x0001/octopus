package gin

import (
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

const defaultMultipartMemory = 32 << 20 // 表单解析内存上限 32MB。

const (
	jsonType   = "application/json; charset=utf-8"
	plainType  = "text/plain; charset=utf-8"
	bindingTag = "binding"
)

// ShouldBindJSON 从请求体解码 JSON 到 obj 并执行 binding tag 校验。
// 支持 required、omitempty、min、oneof 规则; 未知规则静默忽略,
// 不因规则缺失而误拒合法请求。
func (c *Context) ShouldBindJSON(obj any) error {
	if c.Request == nil || c.Request.Body == nil {
		return errors.New("invalid request")
	}
	if err := json.NewDecoder(c.Request.Body).Decode(obj); err != nil {
		return err
	}
	return validateBinding(obj)
}

// JSON 渲染: 状态码 + Content-Type(仅在未设置时写入) + JSON 体。
// 使用 json.Encoder 渲染(自带结尾换行)。
func (c *Context) JSON(code int, obj any) {
	c.Writer.WriteHeader(code)
	writeContentType(c.Writer, jsonType)
	if err := json.NewEncoder(c.Writer).Encode(obj); err != nil {
		panic(err)
	}
}

// String 渲染纯文本响应。
func (c *Context) String(code int, s string) {
	c.Writer.WriteHeader(code)
	writeContentType(c.Writer, plainType)
	if _, err := c.Writer.WriteString(s); err != nil {
		panic(err)
	}
}

// Data 以指定 Content-Type 渲染原始字节响应。
func (c *Context) Data(code int, contentType string, data []byte) {
	c.Writer.WriteHeader(code)
	writeContentType(c.Writer, contentType)
	if _, err := c.Writer.Write(data); err != nil {
		panic(err)
	}
}

// AbortWithStatusJSON 中止链并渲染 JSON 错误响应。
func (c *Context) AbortWithStatusJSON(code int, obj any) {
	c.Abort()
	c.JSON(code, obj)
}

// FormFile 返回 multipart 表单中的文件; 首次调用时解析表单。
func (c *Context) FormFile(name string) (*multipart.FileHeader, error) {
	if c.Request.MultipartForm == nil {
		if err := c.Request.ParseMultipartForm(defaultMultipartMemory); err != nil {
			return nil, err
		}
	}
	_, header, err := c.Request.FormFile(name)
	return header, err
}

// writeContentType 仅在 Content-Type 未设置时写入, handler 预设的类型优先。
func writeContentType(w http.ResponseWriter, value string) {
	header := w.Header()
	if value := header.Get("Content-Type"); value != "" {
		return
	}
	header.Set("Content-Type", value)
}

// validateBinding 按 binding tag 校验结构体(仅顶层字段, 与上游用法一致)。
func validateBinding(obj any) error {
	value := reflect.ValueOf(obj)
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return nil
	}
	var errs []string
	t := value.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get(bindingTag)
		if tag == "" || tag == "-" {
			continue
		}
		fieldValue := value.Field(i)
		rules := strings.Split(tag, ",")
		if isEmptyValue(fieldValue) {
			if len(rules) > 0 && rules[0] == "omitempty" {
				continue
			}
			if containsRule(rules, "required") {
				errs = append(errs, field.Name+" is required")
			}
			continue
		}
		if !passesRules(rules[1:], fieldValue, t.Field(i).Name) {
			errs = append(errs, field.Name+" failed binding: "+tag)
		}
	}
	if len(errs) > 0 {
		return &bindingErrors{messages: errs}
	}
	return nil
}

func containsRule(rules []string, name string) bool {
	for _, r := range rules {
		if r == name {
			return true
		}
	}
	return false
}

// passesRules 逐条检查非空字段值是否满足剩余规则; 未知规则忽略。
func passesRules(rules []string, value reflect.Value, name string) bool {
	for _, rule := range rules {
		switch {
		case rule == "":
			continue
		case strings.HasPrefix(rule, "min="):
			limit, err := strconv.ParseFloat(strings.TrimPrefix(rule, "min="), 64)
			if err != nil {
				continue
			}
			if !atLeastMin(value, limit) {
				return false
			}
		case strings.HasPrefix(rule, "oneof="):
			options := strings.Fields(strings.TrimPrefix(rule, "oneof="))
			if !inOptions(value, options) {
				return false
			}
		default:
			// required 已在上层处理; 其余未实现规则静默忽略。
		}
	}
	return true
}

// atLeastMin 检查 min 规则: 数值比大小, 字符串与集合比长度。
func atLeastMin(value reflect.Value, limit float64) bool {
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(value.Int()) >= limit
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(value.Uint()) >= limit
	case reflect.Float32, reflect.Float64:
		return value.Float() >= limit
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return float64(value.Len()) >= limit
	default:
		return true
	}
}

// inOptions 检查 oneof 规则: 值必须在空格分隔的枚举内。
func inOptions(value reflect.Value, options []string) bool {
	switch value.Kind() {
	case reflect.String:
		text := value.String()
		for _, option := range options {
			if text == option {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// isEmptyValue 判断零值语义: required 视为空即缺, omitempty 视为空即跳过。
func isEmptyValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.String:
		return value.String() == ""
	case reflect.Slice, reflect.Map:
		return value.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return value.IsNil()
	case reflect.Bool:
		return !value.Bool()
	default:
		// 数值零与其他零值类型均视为空, 与 validator 的 hasValue 相反语义一致。
		return value.IsZero()
	}
}

// bindingErrors 聚合校验失败信息; 调用方只判 error 是否为 nil,
// 消息文本不构成契约。
type bindingErrors struct {
	messages []string
}

func (e *bindingErrors) Error() string { return strings.Join(e.messages, "; ") }
