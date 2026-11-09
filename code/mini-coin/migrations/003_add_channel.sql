-- 演示: 给已有大表加字段。生产上先评估锁表时间, 大表常用 online DDL/gh-ost。
ALTER TABLE recharge_order
  ADD COLUMN channel VARCHAR(16) NOT NULL DEFAULT 'unknown' AFTER amount_cent,
  ALGORITHM=INPLACE, LOCK=NONE;
