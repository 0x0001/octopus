package gin_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

type respBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func setupGin() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.GET("/api/v1/static/ping", func(c *gin.Context) { c.String(200, "pong") })
	r.GET("/api/v1/group/get/:id", func(c *gin.Context) {
		id := c.Param("id")
		c.JSON(200, respBody{Code: 200, Message: "success", Data: id})
	})
	r.POST("/api/v1/group/create", func(c *gin.Context) {
		var req struct {
			Name    string `json:"name" binding:"required"`
			Mode    string `json:"mode" binding:"omitempty,oneof=manual failover"`
			Options []int  `json:"options" binding:"omitempty,min=1"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(400, respBody{Code: 400})
			return
		}
		c.JSON(200, respBody{Code: 200, Data: req.Name})
	})
	return r
}

func benchRoute(b *testing.B, path string, withBody bool, body string) {
	engine := setupGin()
	server := httptest.NewServer(engine)
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var resp *http.Response
			var err error
			if withBody {
				resp, err = client.Post(server.URL+path, "application/json", nil)
			} else {
				resp, err = client.Get(server.URL + path)
			}
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkGin_Static(b *testing.B) { benchRoute(b, "/api/v1/static/ping", false, "") }
func BenchmarkGin_Param(b *testing.B)  { benchRoute(b, "/api/v1/group/get/12345", false, "") }
func BenchmarkGin_BindJSON(b *testing.B) {
	benchRoute(b, "/api/v1/group/create", true, `{"name":"g","mode":"manual","options":[1,2]}`)
}
func BenchmarkGin_JSONEncodeOnly(b *testing.B) {
	_ = setupGin
	rec := httptest.NewRecorder()
	c := &gin.Context{Writer: gin.NewResponseWriter(rec), Request: httptest.NewRequest("GET", "/", nil)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.JSON(200, respBody{Code: 200, Message: "success", Data: strconv.Itoa(i)})
	}
	_ = json.Marshal
	_ = rec
}
