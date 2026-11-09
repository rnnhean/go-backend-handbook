# Plan: 充值接口

1. 建 account / recharge_order 表与唯一索引。
2. 实现 repository 与 service。
3. 加 HTTP handler 与参数校验。
4. 补单元测试和 curl 自测。
5. 观察 QPS、错误率、P99 后再放量。
