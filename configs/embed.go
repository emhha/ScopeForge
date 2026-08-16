// Package configs 内嵌随二进制分发的默认配置样例:
// 单二进制部署时(部署目录没有源码 configs/),首次启动生成的
// <dataDir>/scopeforge.yaml 由此写入,无需连带源码一起分发。
package configs

import _ "embed"

// Example 是 scopeforge.yaml.example 的原始字节(随二进制分发)。
//
//go:embed scopeforge.yaml.example
var Example []byte
