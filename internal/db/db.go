package db

import (
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type Config struct {
	ID      uint `gorm:"primaryKey"`
	APIKey  string
	Model   string
	BaseURL string
}

type UserProfile struct {
	ID            uint `gorm:"primaryKey"`
	Name          string
	About         string `gorm:"type:text"`
	FamilyContext string `gorm:"type:text"`
	WorkContext   string `gorm:"type:text"`
	WritingStyle  string `gorm:"type:text"`
}

type MessengerConfig struct {
	ID       uint   `gorm:"primaryKey"`
	Type     string // "signal", "whatsapp", "telegram", etc.
	Phone    string
	APIToken string
	Enabled  bool `gorm:"default:false"`
}

type Contact struct {
	ID          string `gorm:"primaryKey"`
	Name        string
	Phone       string
	Messenger   string
	Enabled     bool
	Description string `gorm:"type:text"`
	Style       string `gorm:"type:text"`
}

type Message struct {
	ID        string `gorm:"primaryKey"`
	ContactID string
	Content   string `gorm:"type:text"`
	Type      string
	MediaURL  string
	Timestamp int64
	IsFromMe  bool
	Reaction  string
}

var DB *gorm.DB

func Init(dataPath string) error {
	if err := os.MkdirAll(dataPath, 0o750); err != nil {
		return err
	}

	dbPath := filepath.Join(dataPath, "talktothem.db")

	var err error
	DB, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		return err
	}

	if err := DB.AutoMigrate(&Config{}, &MessengerConfig{}, &Contact{}, &Message{}, &UserProfile{}); err != nil {
		return err
	}

	var configCount int64
	DB.Model(&Config{}).Count(&configCount)
	if configCount == 0 {
		DB.Create(&Config{})
	}

	return nil
}

func GetConfig() *Config {
	var config Config
	DB.First(&config)
	return &config
}

func UpdateConfig(config *Config) error {
	return DB.Save(config).Error
}

func GetOrCreateConfig() *Config {
	var config Config
	result := DB.First(&config)
	if result.Error == gorm.ErrRecordNotFound {
		config = Config{}
		DB.Create(&config)
	}
	return &config
}

func GetMessengerConfig(messengerType string) *MessengerConfig {
	var config MessengerConfig
	result := DB.Where("type = ?", messengerType).First(&config)
	if result.Error != nil {
		return nil
	}
	return &config
}

func SaveMessengerConfig(config *MessengerConfig) error {
	return DB.Save(config).Error
}

func GetUserProfile() *UserProfile {
	var profile UserProfile
	result := DB.First(&profile)
	if result.Error == gorm.ErrRecordNotFound {
		return &UserProfile{}
	}
	return &profile
}

func UpdateUserProfile(profile *UserProfile) error {
	if profile.ID == 0 {
		return DB.Create(profile).Error
	}
	return DB.Save(profile).Error
}
