package httpapi

import "github.com/gin-gonic/gin"

type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Code      string `json:"code"`
	RequestID string `json:"requestId,omitempty"`
}

func writeProblem(c *gin.Context, status int, code, title, detail string) {
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(status, Problem{
		Type:      "https://wenzwork.example/problems/" + code,
		Title:     title,
		Status:    status,
		Detail:    detail,
		Code:      code,
		RequestID: requestIDFrom(c),
	})
}
