// Package config loads environment configuration, paths, and external tool locations.
package config

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const DefaultListen = "127.0.0.1:8420"

type Config struct {
	RootDir        string
	DataDir        string
	EnvPath        string
	ListenAddr     string
	YtDlpPath      string
	ServiceAccount []byte
	VertexLocation string
	QwenURL        string
	QwenModel      string
}

func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// AppRoot is the radio-station directory: RADIO_ROOT, else the parent of the
// directory holding this binary (built into <root>/aiserver/).
func AppRoot() string {
	if v := os.Getenv("RADIO_ROOT"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("locate executable: %v", err)
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(filepath.Dir(exe))
}

func ResolveYtDlp(root string) string {
	if v := os.Getenv("YTDLP"); v != "" {
		return v
	}
	if p := filepath.Join(root, "bin", "yt-dlp"); FileExists(p) {
		return p
	}
	if p, err := exec.LookPath("yt-dlp"); err == nil {
		return p
	}
	return ""
}

func FileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// parseDotEnvLine parses a single KEY=VALUE line from a dotenv file.
func parseDotEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:eq])
	v := strings.TrimSpace(line[eq+1:])
	if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return k, v, k != ""
}

// LoadDotEnvFile loads any unset environment variables from a dotenv file.
func LoadDotEnvFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := parseDotEnvLine(line); ok && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// LoadEnvValue reads a single KEY from a dotenv file without sourcing it.
func LoadEnvValue(path, key string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := parseDotEnvLine(line); ok && k == key {
			return v, true
		}
	}
	return "", false
}
