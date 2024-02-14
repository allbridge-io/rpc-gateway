package admin

type AdminServerConfig struct {
	Port uint `yaml:"port"`
}

type Config struct { //nolint:revive
	AdminServerConfig  AdminServerConfig      `yaml:"admin"`
}
