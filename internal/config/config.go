package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram  TelegramConfig  `yaml:"telegram"`
	Access    AccessConfig    `yaml:"access"`
	Paths     PathsConfig     `yaml:"paths"`
	Git       GitConfig       `yaml:"git"`
	Narrative NarrativeConfig `yaml:"narrative"`
}

type TelegramConfig struct {
	Token          string `yaml:"token"`
	PollingTimeout int    `yaml:"polling_timeout"`
	ParseMode      string `yaml:"parse_mode"`
}

type AccessConfig struct {
	AllowedUserIDs []int `yaml:"allowed_user_ids"`
}

type PathsConfig struct {
	DataRoot   string `yaml:"data_root"`
	GitWorkdir string `yaml:"git_workdir"`
}

type GitConfig struct {
	Remote       string `yaml:"remote"`
	Branch       string `yaml:"branch"`
	CommitAuthor string `yaml:"commit_author"`
	CommitEmail  string `yaml:"commit_email"`
}

type NarrativeConfig struct {
	WordLimit int    `yaml:"word_limit"`
	Language  string `yaml:"language"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	cfg.resolveRelativePaths(filepath.Dir(path))
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Telegram.Token == "" || c.Telegram.Token == "REPLACE_WITH_BOTFATHER_TOKEN" {
		return errors.New("telegram.token must be set to a real bot token")
	}
	if len(c.Access.AllowedUserIDs) == 0 {
		return errors.New("access.allowed_user_ids must contain at least one user id")
	}
	if c.Paths.DataRoot == "" {
		c.Paths.DataRoot = "game-data"
	}
	if c.Paths.GitWorkdir == "" {
		c.Paths.GitWorkdir = "."
	}
	if c.Git.Remote == "" {
		c.Git.Remote = "origin"
	}
	if c.Git.Branch == "" {
		c.Git.Branch = "master"
	}
	if c.Narrative.WordLimit == 0 {
		c.Narrative.WordLimit = 350
	}
	if c.Narrative.Language == "" {
		c.Narrative.Language = "ru"
	}
	return nil
}

func (c *Config) resolveRelativePaths(base string) {
	if !filepath.IsAbs(c.Paths.DataRoot) {
		c.Paths.DataRoot = filepath.Join(base, c.Paths.DataRoot)
	}
	if !filepath.IsAbs(c.Paths.GitWorkdir) {
		c.Paths.GitWorkdir = filepath.Join(base, c.Paths.GitWorkdir)
	}
}

func (c *Config) IsAllowed(userID int) bool {
	for _, id := range c.Access.AllowedUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}
