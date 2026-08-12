package auth

type Config struct {
	Basic BasicConfig
}

type BasicConfig struct {
	Username string
	Password string
}
