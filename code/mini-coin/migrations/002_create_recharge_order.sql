CREATE TABLE recharge_order (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  req_id     VARCHAR(64)     NOT NULL,
  uid        VARCHAR(64)     NOT NULL,
  amount_cent BIGINT         NOT NULL,
  status     TINYINT         NOT NULL DEFAULT 0 COMMENT '0处理中 1成功 2失败',
  created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_req_id (req_id),
  KEY idx_uid_created (uid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
