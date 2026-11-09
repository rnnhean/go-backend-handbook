# Changelog

## 1.2.1

- 修正 `package.json` 的 `repository`、`homepage`、`bugs` 字段，指向真实 GitHub 仓库。
- README 的 CDN 阅读入口同步到 1.2.1。

## 1.2.0

- 重写第 11 章监控看板：五层 Grafana 布局逐面板解释指标、PromQL、阈值和告警处置。
- 重写第 16、24 章接口文档与自测：统一 `mini-coin` 的 `req_id/amount_cent/channel` 契约、201/200 幂等语义、错误码、curl、Postman、httptest 和契约测试。
- 补充第 23、25 章的排障操作、证据链、发布回滚和运行手册。
- 对齐第 02、08、09、12、13、15、18、21 章中的示例端口、鉴权、请求字段和错误码。
- 为 `mini-coin` 补充金额上限、`req_id` 长度、`channel` 白名单、账户不存在 404 及对应测试。
- 修正安全示例中的 `http.MaxBytesReader` 用法，并完成公开内容脱敏检查。

## 1.1.1

- 扩充监控、测试、性能、安全和生产运维章节。

## 1.1.0

- 增加深度实战卷与完整 `mini-coin` 示例项目。
