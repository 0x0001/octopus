//go:build !postgres

// 默认构建的 PostgreSQL 占位实现: 驱动不编译进二进制, 请求时给出明确指引。

package db

import (
	"errors"

	"gorm.io/gorm"
)

// errPostgresNotBuilt 表示本二进制未启用 PostgreSQL 支持。
var errPostgresNotBuilt = errors.New(`postgresql 支持未编译进本二进制; 需要时请以 -tags postgres 构建, 或改用 sqlite/mysql`)

// initPostgres 在未启用 postgres 构建标签时直接报错。
func initPostgres(_ string, _ *gorm.Config) (*gorm.DB, error) {
	return nil, errPostgresNotBuilt
}
