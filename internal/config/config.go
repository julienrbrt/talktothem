package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

type Config struct {
	Signal  SignalConfig  `mapstructure:"signal"`
	Agent   AgentConfig   `mapstructure:"agent"`
	Contact ContactConfig `mapstructure:"contact"`
}

type SignalConfig struct {
	PhoneNumber string `mapstructure:"phone_number"`
	APIURL      string `mapstructure:"api_url"`
}

type AgentConfig struct {
	BaseURL string `mapstructure:"base_url"`
	Model   string `mapstructure:"model"`
	APIKey  string `mapstructure:"api_key"`
}

type ContactConfig struct {
	DataPath string `mapstructure:"data_path"`
}

func Load(cfgFile string) (*Config, error) {
	viper.SetEnvPrefix("talktothem")
	viper.AutomaticEnv()
	viper.BindEnv("signal.api_url", "TALKTOTHEM_SIGNAL_API_URL")
	viper.BindEnv("signal.phone_number", "TALKTOTHEM_SIGNAL_PHONE_NUMBER")
	viper.BindEnv("agent.api_key", "TALKTOTHEM_AGENT_API_KEY")
	viper.BindEnv("agent.model", "TALKTOTHEM_AGENT_MODEL")
	viper.BindEnv("agent.base_url", "TALKTOTHEM_AGENT_BASE_URL")
	viper.BindEnv("contact.data_path", "TALKTOTHEM_CONTACT_DATA_PATH")

	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}

		viper.AddConfigPath(filepath.Join(home, ".config", "talktothem"))
		viper.AddConfigPath(".")
		viper.SetConfigType("yaml")
		viper.SetConfigName("config")
	}

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	setDefaults(&cfg)

	return &cfg, nil
}

func setDefaults(cfg *Config) {
	if cfg.Signal.APIURL == "" {
		cfg.Signal.APIURL = "http://localhost:8080"
	}
	if cfg.Contact.DataPath == "" {
		home, _ := os.UserHomeDir()
		cfg.Contact.DataPath = filepath.Join(home, ".config", "talktothem", "contacts")
	}
}

func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "talktothem", "config.yaml"), nil
}
