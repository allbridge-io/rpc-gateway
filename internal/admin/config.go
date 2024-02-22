package admin

type AdminServerConfig struct {
	Admins           []string    `yaml:"admins"`
	BasePath         string      `yaml:"basePath"`
	Cors             CorsOptions `yaml:"cors"`
	Domain           string      `yaml:"domain"`
	MaxTokenLifespan uint        `yaml:"maxTokenLifespan"`
	Port             uint        `yaml:"port"`
}

type CorsOptions struct { //nolint:revive
	AllowedOrigins string `yaml:"allowedOrigins"`
}

type Config struct { //nolint:revive
	AdminServerConfig  AdminServerConfig      `yaml:"admin"`
}
