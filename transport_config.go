package main

import (
	"strings"

	"gopkg.in/yaml.v3"
)

func (p *pluginRuntime) configureTransport(raw []byte) error {
	var cfg struct {
		Transport string `yaml:"http-transport"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return failure(400, "Invalid Cursor plugin configuration")
	}
	mode := strings.TrimSpace(cfg.Transport)
	if mode != "" && mode != "http1" && mode != "host-default" {
		return failure(400, "Cursor http-transport must be http1 or host-default")
	}
	p.mu.Lock()
	p.hostDefaultTransport = mode == "host-default"
	p.mu.Unlock()
	return nil
}
