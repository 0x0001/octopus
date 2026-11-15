//go:build mysql

// MySQL 驱动实现, 仅在构建标签 mysql 下编译。

package db

import (
	"strings"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// initMySQL 初始化 MySQL 连接。
// DSN 格式: user:password@tcp(host:port)/dbname?charset=utf8mb4&parseTime=True&loc=Local
func initMySQL(dsn string, config *gorm.Config) (*gorm.DB, error) {
	if !strings.Contains(dsn, "?") {
		dsn += "?charset=utf8mb4&parseTime=True&loc=Local"
	}
	return gorm.Open(mysql.Open(dsn), config)
}
