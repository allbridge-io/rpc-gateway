package admin

type AdminServerConfig struct {
	Port uint `yaml:"port"`
	Admins []string `yaml:"admins"`
	MaxTokenLifespan uint `yaml:"maxTokenLifespan"`
	Domain string `yaml:"domain"`
	BasePath string `yaml:"basePath"`
	Cors CorsOptions `yaml:"cors"`
}

type CorsOptions struct { //nolint:revive
	AllowedOrigins string `yaml:"allowedOrigins"`
}

type Config struct { //nolint:revive
	AdminServerConfig  AdminServerConfig      `yaml:"admin"`
}
