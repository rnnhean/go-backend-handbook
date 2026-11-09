package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Address   string          `json:"address"`
	MySQLDSN  string          `json:"mysql_dsn"`
	RedisAddr string          `json:"redis_addr"`
	MQBrokers []string        `json:"mq_brokers"`
	Features  map[string]bool `json:"features"`
	LogLevel  string          `json:"log_level"`
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return c, nil
}
