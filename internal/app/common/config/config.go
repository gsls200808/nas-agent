package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// AppConfig 应用配置
type AppConfig struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	Log     LogConfig     `yaml:"log"`
	Admin   AdminConfig   `yaml:"admin"`
}

type ServerConfig struct {
	Port int    `yaml:"port"`
	Mode string `yaml:"mode"`
}

type StorageConfig struct {
	Root        string `yaml:"root"`
	Database    string `yaml:"database"`
	DownloadDir string `yaml:"downloadDir"` // 下载转存任务的本地暂存目录（默认系统临时目录）
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type AdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

func Default() *AppConfig {
	return &AppConfig{
		Server:  ServerConfig{Port: 8080, Mode: "release"},
		Storage: StorageConfig{Root: "./data/storage", Database: "./data/nas-agent.db", DownloadDir: os.TempDir()},
		Log:     LogConfig{Level: "info"},
		Admin:   AdminConfig{Username: "admin", Password: "admin123"},
	}
}

// Load 加载 YAML 配置
func Load(path string) (*AppConfig, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
