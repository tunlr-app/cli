// Package config 实现配置流水线：订阅 + 模板 + mixin 合成、mihomo -t 校验、
// 原子写入、回滚与历史。合并基于 yaml.Node，以保留键顺序与注释。
package config
