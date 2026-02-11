package domain

// AppSetting — ключ-значение для настроек (канал ОП, счётчик подписок и т.д.)
type AppSetting struct {
	Key   string `gorm:"primaryKey;size:64" json:"key"`
	Value string `gorm:"type:text" json:"value"`
}

func (AppSetting) TableName() string {
	return "app_settings"
}
