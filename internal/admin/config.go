package admin

type AdminServerConfig struct {
	Port uint `yaml:"port"`
	BasePath string `yaml:"basePath"`
}

type Config struct { //nolint:revive
	AdminServerConfig  AdminServerConfig      `yaml:"admin"`
}
