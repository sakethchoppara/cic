// Package config loads language definitions from languages.yaml.
package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

const defaultCompilePids = 256

type Lang struct {
	Image           string   `yaml:"image"`
	User            string   `yaml:"user"`
	SourceFile      string   `yaml:"source_file"`
	CompileCmd      []string `yaml:"compile_cmd"`
	RunCmd          []string `yaml:"run_cmd"`
	Env             []string `yaml:"env"`
	CompileMemoryMB int      `yaml:"compile_memory_mb"`
	RunMemoryMB     int      `yaml:"run_memory_mb"`
	CompilePids     int      `yaml:"compile_pids"`
	TmpfsMB         int      `yaml:"tmpfs_mb"`
}

func Load(path string) (map[string]Lang, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var langs map[string]Lang
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a typo'd key is an error, not a silently ignored limit
	if err := dec.Decode(&langs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, l := range langs {
		if l.Image == "" || l.User == "" || l.SourceFile == "" || len(l.RunCmd) == 0 || l.RunMemoryMB <= 0 || l.TmpfsMB <= 0 {
			return nil, fmt.Errorf("%s: language %q needs image, user, source_file, run_cmd, run_memory_mb, tmpfs_mb", path, name)
		}
		if len(l.CompileCmd) > 0 && l.CompileMemoryMB <= 0 {
			return nil, fmt.Errorf("%s: language %q has compile_cmd but no compile_memory_mb", path, name)
		}
		if l.CompilePids == 0 {
			l.CompilePids = defaultCompilePids
		}
		langs[name] = l
	}
	return langs, nil
}
