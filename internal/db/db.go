package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"github.com/julienrbrt/talktothem/internal/messenger"
	"gorm.io/gorm"
)

type Config struct {
	ID           uint `gorm:"primaryKey"`
	APIKey       string
	Model        string
	BaseURL      string
	DisableDelay bool
}

type UserProfile struct {
	ID            uint `gorm:"primaryKey"`
	Name          string
	About         string `gorm:"type:text"`
	FamilyContext string `gorm:"type:text"`
	WorkContext   string `gorm:"type:text"`
	WritingStyle  string `gorm:"type:text"`
	Location      string
	Timezone      string
	Language      string
}

type MessengerConfig struct {
	ID       uint   `gorm:"primaryKey"`
	Type     string `gorm:"uniqueIndex"` // "signal", "whatsapp", etc.
	APIToken string
	Enabled  bool `gorm:"default:false"`
}

type Contact struct {
	ID           string `gorm:"primaryKey"`
	Name         string
	Phone        string
	Messenger    string
	Enabled      bool
	Description  string `gorm:"type:text"`
	Style        string `gorm:"type:text"`
	Relation     string `gorm:"type:text"`
	BannedTopics string `gorm:"type:text"`
}

type Message struct {
	ID        string `gorm:"primaryKey"`
	ContactID string
	Content   string `gorm:"type:text"`
	Type      string
	MediaURLs string `gorm:"type:text"` // Comma separated URLs
	Timestamp int64
	IsFromMe  bool
	Reaction  string
	IsGroup   bool
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

func GetMessengerConfig(messengerType string) *MessengerConfig {
	var config MessengerConfig
	result := DB.Where("type = ?", messengerType).Order("enabled DESC, id DESC").First(&config)
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

func PrefillProfileFromMessenger(ctx context.Context, msgr messenger.Messenger, messengerName string) error {
	profile, err := msgr.GetOwnProfile(ctx)
	if err != nil {
		return err
	}

	if profile.Name == "" && profile.About == "" {
		return nil
	}

	existing := GetUserProfile()
	updated := false

	if existing.Name == "" && profile.Name != "" {
		existing.Name = profile.Name
		updated = true
	}
	if existing.About == "" && profile.About != "" {
		existing.About = profile.About
		updated = true
	}

	if !updated {
		return nil
	}

	return UpdateUserProfile(existing)
}

func UpdateLearnedFields(location, timezone, language string) error {
	existing := GetUserProfile()
	updated := false

	if existing.Location == "" && location != "" {
		existing.Location = location
		updated = true
	}
	if existing.Timezone == "" && timezone != "" {
		existing.Timezone = timezone
		updated = true
	}
	if existing.Language == "" && language != "" {
		existing.Language = language
		updated = true
	}

	if !updated {
		return nil
	}

	return UpdateUserProfile(existing)
}

func PhoneRegionHint(phone string) string {
	phone = strings.TrimPrefix(phone, "+")
	regions := map[string]string{
		"1":   "United States / Canada",
		"44":  "United Kingdom",
		"49":  "Germany",
		"39":  "Italy",
		"34":  "Spain",
		"55":  "Brazil",
		"91":  "India",
		"81":  "Japan",
		"86":  "China",
		"61":  "Australia",
		"33":  "France",
		"41":  "Switzerland",
		"32":  "Belgium",
		"31":  "Netherlands",
		"46":  "Sweden",
		"47":  "Norway",
		"45":  "Denmark",
		"351": "Portugal",
		"358": "Finland",
	}

	for prefix, region := range regions {
		if strings.HasPrefix(phone, prefix) {
			return region
		}
	}
	return ""
}
