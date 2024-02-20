package admin

type AdminServerConfig struct {
	Admins           []string `yaml:"admins"`
	BasePath         string   `yaml:"basePath"`
	Domain           string   `yaml:"domain"`
	MaxTokenLifespan uint     `yaml:"maxTokenLifespan"`
	Port             uint     `yaml:"port"`
}

type Config struct { //nolint:revive
	AdminServerConfig  AdminServerConfig      `yaml:"admin"`
}
