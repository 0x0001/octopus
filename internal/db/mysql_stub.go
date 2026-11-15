//go:build !mysql

// 默认构建的 MySQL 占位实现: 驱动不编译进二进制, 请求时给出明确指引。

package db

import (
	"errors"

	"gorm.io/gorm"
)

// errMySQLNotBuilt 表示本二进制未启用 MySQL 支持。
var errMySQLNotBuilt = errors.New(`mysql 支持未编译进本二进制; 需要时请以 -tags mysql 构建, 或改用 sqlite`)

// initMySQL 在未启用 mysql 构建标签时直接报错。
func initMySQL(_ string, _ *gorm.Config) (*gorm.DB, error) {
	return nil, errMySQLNotBuilt
}
