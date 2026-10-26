// Package netfilter 负责 nftables 规则的生成、应用与清理，以及开启 TUN 前的系统前置检查。
// 规则由模板生成脚本，经 nft -f 原子加载。
package netfilter
