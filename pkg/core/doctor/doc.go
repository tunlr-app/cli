// Package doctor 实现环境诊断：检查本机是否满足支持条件（systemd、架构、root、不在 WSL 或容器中等）、
// 能否开启透明代理、网络是否可用。不满足时明确拒绝并说明原因，不做兜底兼容。
//
// 本包只做与安装无关的检查；安装是否健康由 setup 汇总（它需要状态、设置、unit 等），
// 整份报告见 setup.Installer.Doctor。init 和 tun on 复用这里的检查。
package doctor
