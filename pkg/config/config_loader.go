// Copyright 2024 The Authors (see AUTHORS file)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"time"

	"github.com/google/cel-go/cel"
	"gopkg.in/yaml.v3"

	"github.com/abcxyz/github-token-minter/pkg/server/source"
	"github.com/abcxyz/pkg/cache"
)

// ConfigFileLoader represents an object that is capable of
// retrieving a configuration files contents in its raw form.
type ConfigFileLoader interface {
	// Source is a function that returns the location where the loader expects
	// that it will found the configuration for a given org/repo combination.
	Source(org, repo string) string
	// Load reads the configuration for an org and repo and marshals
	// it into a Config object.
	Load(ctx context.Context, org, repo string) (*Config, error)
}

type cachingConfigFileLoader struct {
	loader ConfigFileLoader
	cache  *cache.Cache[*Config]
}

func newCachingConfigLoader(expireAfter time.Duration, child ConfigFileLoader) ConfigFileLoader {
	return &cachingConfigFileLoader{
		loader: child,
		cache:  cache.New[*Config](expireAfter),
	}
}

type compilingConfigLoader struct {
	loader ConfigFileLoader
	env    *cel.Env
}

func NewCompilingConfigLoader(env *cel.Env, child ConfigFileLoader) ConfigFileLoader {
	return &compilingConfigLoader{
		loader: child,
		env:    env,
	}
}

func (l *compilingConfigLoader) Load(ctx context.Context, org, repo string) (*Config, error) {
	log.Printf("*** compilingConfigLoader.Load (org: %q, repo: %q)", org, repo)

	cfg, err := l.loader.Load(ctx, org, repo)
	if err != nil {
		return nil, fmt.Errorf("compiling config loader, sub loader failed to load configuration: %w", err)
	}
	if cfg != nil {
		if err := cfg.compile(l.env); err != nil {
			return nil, fmt.Errorf("failed to compile CEL expressions in config: %w", err)
		}
	}
	log.Printf("*** compilingConfigLoader.Load (org: %q, repo: %q) cfg: %+v", org, repo, configStr(cfg))

	return cfg, nil
}

func (l *compilingConfigLoader) Source(org, repo string) string {
	return l.loader.Source(org, repo)
}

func (l *cachingConfigFileLoader) Load(ctx context.Context, org, repo string) (*Config, error) {
	log.Printf("*** cachingConfigFileLoader.Load (org: %q, repo: %q)", org, repo)

	key := fmt.Sprintf("%s/%s", org, repo)
	// first look for the config object in cache
	cached, exists := l.cache.Lookup(key)
	if exists {
		return cached, nil
	}
	cfg, err := l.loader.Load(ctx, org, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration file for [%s / %s]. Error: %w", org, repo, err)
	}
	l.cache.Set(key, cfg)
	log.Printf("*** cachingConfigFileLoader.Load (org: %q, repo: %q) cfg: %+v", org, repo, configStr(cfg))

	return cfg, nil
}

func (l *cachingConfigFileLoader) Source(org, repo string) string {
	return l.loader.Source(org, repo)
}

// localConfigFileLoader is a configFileLoader implementation that
// reads files from the local file system.
type localConfigFileLoader struct {
	configDir string
}

// Load reads the contents of configuration files from the local file system.
func (l *localConfigFileLoader) Load(ctx context.Context, org, repo string) (*Config, error) {
	log.Printf("*** localConfigFileLoader.Load (org: %q, repo: %q)", org, repo)
	name := fmt.Sprintf("%s/%s/%s.yaml", l.configDir, org, repo)
	data, err := os.ReadFile(name)
	if err != nil {
		// file not found is not an error
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading content from file: %w", err)
	}
	config, err := Read(data)
	if err != nil {
		return nil, fmt.Errorf("error converting raw config bytes into struct: %w", err)
	}
	log.Printf("*** localConfigFileLoader.Load (org: %q, repo: %q) cfg: %+v", org, repo, configStr(config))

	return config, nil
}

func (l *localConfigFileLoader) Source(org, repo string) string {
	return fmt.Sprintf("file://%s/%s/%s.yaml", l.configDir, org, repo)
}

// inRepoConfigFileLoader reads a configuration file from a specific
// path within the requested repository.
type inRepoConfigFileLoader struct {
	sourceSystem source.System
	configPath   string
	ref          string
}

// Load is a configFileLoader implementation that reads the configuration file
// contents from within a GitHub repository.
func (l *inRepoConfigFileLoader) Load(ctx context.Context, org, repo string) (*Config, error) {
	log.Printf("*** inRepoConfigFileLoader.Load (org: %q, repo: %q)", org, repo)
	fileContents, err := l.sourceSystem.RetrieveFileContents(ctx, org, repo, l.configPath, l.ref)
	if err != nil {
		return nil, fmt.Errorf("error reading configuration file contents @ %s/%s/%s: %w", org, repo, l.configPath, err)
	}
	// File not found
	if len(fileContents) == 0 {
		return nil, nil
	}
	config, err := Read(fileContents)
	if err != nil {
		return nil, fmt.Errorf("error converting raw config bytes into struct: %w", err)
	}
	log.Printf("*** inRepoConfigFileLoader.Load (org: %q, repo: %q) cfg: %+v", org, repo, configStr(config))

	return config, nil
}

func (l *inRepoConfigFileLoader) Source(org, repo string) string {
	return fmt.Sprintf("%s/%s/%s/%s", l.sourceSystem.BaseURL(), org, repo, l.configPath)
}

// fixedRepoConfigFileLoader reads the contents of a configuration file from
// a specific repository, not from the target repository. It wraps another
// loader and delegates the retrieval to that implementation after replacing
// the repo parameter.
type fixedRepoConfigFileLoader struct {
	loader ConfigFileLoader
	repo   string
}

// Load is a configFileLoader implementation that retrieves the contents of a
// configuration file from a specific repository in the org, not from the target repository.
func (l *fixedRepoConfigFileLoader) Load(ctx context.Context, org, repo string) (*Config, error) {
	log.Printf("*** fixedRepoConfigFileLoader.Load (org: %q, repo: %q)", org, repo)
	res, err := l.loader.Load(ctx, org, l.repo)
	if err != nil {
		return nil, fmt.Errorf("error reading config file from child loader: %w", err)
	}
	log.Printf("*** fixedRepoConfigFileLoader.Load (org: %q, repo: %q) cfg: %+v", org, repo, configStr(res))

	return res, nil
}

func (l *fixedRepoConfigFileLoader) Source(org, repo string) string {
	return l.loader.Source(org, l.repo)
}

func Read(contents []byte) (*Config, error) {
	var config Config
	// default to the latest version of the config format
	if err := yaml.Unmarshal(contents, &config); err != nil {
		// attempt to unmarshal in the old format
		var cv1 v1Document
		if err := yaml.Unmarshal(contents, &cv1); err != nil {
			return nil, fmt.Errorf("error parsing yaml document: %w", err)
		}
		config = NewConfigFromV1(cv1)
	}
	return &config, nil
}

func configStr(cfg *Config) string {
	sb := "\n"
	sb += "****CONFIG START****\n"
	defer func() {
		sb += "****CONFIG END****\n"
	}()
	if cfg == nil {
		return "<nil>\n"
	}
	for name, scope := range cfg.Scopes {
		sb += fmt.Sprintf(" ***Scope %s: %+v\n", name, scope)
		if scope == nil {
			sb += fmt.Sprintf("   ***Scope <nil>\n")
			continue
		}
		for _, r := range scope.Repositories {
			sb += fmt.Sprintf("   *** Repository %q\n", r)
		}
		for perm, action := range scope.Permissions {
			sb += fmt.Sprintf("   *** Permission %q: %q\n", perm, action)
		}
	}
	return sb
}
