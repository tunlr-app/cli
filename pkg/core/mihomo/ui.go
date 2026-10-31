package mihomo

// ZashboardURL 是默认面板 zashboard 最新发布的官方下载地址，模板中的 external-ui-url 指向它。面板由内核按
// external-ui-url 自己下载（加载配置时 external-ui 目录不存在或为空，或在面板上手动更新），tunlr 不下载。
// 用字体由浏览器从 CDN 加载的版本（约 2.9 MB），内置全部字体的 dist.zip 约 9.6 MB，经镜像下载慢得多。
const ZashboardURL = "https://github.com/Zephyruso/zashboard/releases/latest/download/dist-cdn-fonts.zip"
