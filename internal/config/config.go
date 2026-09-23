package config

import (
	"encoding/json"
	"os"
	"strings"
	"fmt"
	"bufio"

	"golang.org/x/crypto/bcrypt"
)

// Users / Auth configuration (saved to /etc/peakroute/auth.json with permissions 0600)
type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
}

type AuthConfig struct {
	Users []User `json:"users,omitempty"`
}

// Router Network configuration (saved to /etc/peakroute/network.json)
type SystemInterface struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

type NetworkConfig struct {
	WanInterface  string            `json:"wan_interface,omitempty"`
	LanInterfaces []string          `json:"lan_interfaces,omitempty"`
	LanIP         string            `json:"lan_ip,omitempty"`
	Interfaces    []SystemInterface `json:"interfaces,omitempty"`
}

type SysConfig struct {
	OperationMode string `json:"operation_mode"` // "ROUTER" or "AP"
}

// Password helpers stay the same...
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

func CheckPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// Dedicated Auth file operations
func SaveAuthConfig(filepath string, cfg *AuthConfig) error {
	data, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath, data, 0600)
}

func LoadAuthConfig(filepath string) (*AuthConfig, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	var cfg AuthConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Dedicated Network file operations
func SaveNetworkConfig(filepath string, cfg *NetworkConfig) error {
	data, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath, data, 0644)
}

func LoadNetworkConfig(filepath string) (*NetworkConfig, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	var cfg NetworkConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func LoadSysConfig(filepath string) (*SysConfig, error) {
	file, err := os.Open(filepath)
	if err != nil {
		// Fallback default if sys.conf isn't generated yet
		return &SysConfig{OperationMode: "ROUTER"}, nil
	}
	defer file.Close()

	cfg := &SysConfig{
		OperationMode: "ROUTER",
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(strings.ToUpper(parts[1]))
			val = strings.Trim(val, "\"'")

			if key == "OPERATION_MODE" || key == "MODE" {
				cfg.OperationMode = val
			}
		}
	}

	return cfg, scanner.Err()
}

func SaveSysConfig(filepath string, cfg *SysConfig) error {
	content := fmt.Sprintf("OPERATION_MODE=%s\n", cfg.OperationMode)
	return os.WriteFile(filepath, []byte(content), 0644)
}
