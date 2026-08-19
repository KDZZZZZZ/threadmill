// Package logging 创建 Threadmill 使用的结构化日志记录器。
package logging

import (
	"io"
	"log/slog"
	"os"
)

// Config 配置日志输出、最低级别和编码格式。
type Config struct {
	Output io.Writer
	Level  slog.Level
	JSON   bool
}

// New 创建独立 Logger；默认输出到标准错误，使用 info 级别的文本格式。
func New(config Config) *slog.Logger {
	output := config.Output
	if output == nil {
		output = os.Stderr
	}

	options := &slog.HandlerOptions{Level: config.Level}
	if config.JSON {
		return slog.New(slog.NewJSONHandler(output, options))
	}
	return slog.New(slog.NewTextHandler(output, options))
}
