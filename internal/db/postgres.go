//go:build postgres

// PostgreSQL 驱动实现, 仅在构建标签 postgres 下编译。

package db

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// initPostgres 初始化 PostgreSQL 连接。
// DSN 格式: host=localhost user=postgres password=xxx dbname=octopus port=5432 sslmode=disable
func initPostgres(dsn string, config *gorm.Config) (*gorm.DB, error) {
	return gorm.Open(postgres.Open(dsn), config)
}
