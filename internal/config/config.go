package config

import (
	"encoding/json"
	"os"
	"golang.org/x/crypto/bcrypt"

)

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
}

type SystemInterface struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

type RouterConfig struct {
	WanInterface  string            `json:"wan_interface,omitempty"`
	LanInterfaces []string          `json:"lan_interfaces,omitempty"`
	LanIP         string            `json:"lan_ip,omitempty"`
	Interfaces    []SystemInterface `json:"interfaces,omitempty"`
	Users         []User            `json:"users,omitempty"`
}

func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

func CheckPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

func SaveConfig(filepath string, cfg *RouterConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath, data, 0600)
}

func LoadConfig(filepath string) (*RouterConfig, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	var cfg RouterConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
