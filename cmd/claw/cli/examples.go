package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func helpCmd(args []string) error {
	if len(args) == 0 {
		printHelp()
		return nil
	}
	return configCmdWithOutput(args, os.Stdout)
}

func listRaids() ([]string, error) {
	return listMergedConfigDir("examples/raids", "configs/raid")
}

func listPersonas() ([]string, error) {
	return listMergedConfigDir("examples/personas", "configs/persona")
}

func listTests() ([]string, error) {
	return listMergedNestedConfigDir("examples/test", "configs/test")
}

func listMergedConfigDir(embeddedDir, homeRel string) ([]string, error) {
	embedded, err := listExampleDir(embeddedDir)
	if err != nil {
		return nil, err
	}
	home, err := listClawHomeConfigDir(homeRel)
	if err != nil {
		return nil, err
	}
	return mergeSortedNames(embedded, home), nil
}

func listMergedNestedConfigDir(embeddedDir, homeRel string) ([]string, error) {
	embedded, err := listNestedExampleDir(embeddedDir)
	if err != nil {
		return nil, err
	}
	home, err := listNestedClawHomeConfigDir(homeRel)
	if err != nil {
		return nil, err
	}
	return mergeSortedNames(embedded, home), nil
}

func listExampleDir(dir string) ([]string, error) {
	entries, err := templateFS.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			switch ext {
			case ".yaml", ".yml", ".json":
				base := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
				out = append(out, base)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func listNestedExampleDir(dir string) ([]string, error) {
	groups, err := templateFS.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, group := range groups {
		if !group.IsDir() {
			continue
		}
		groupName := group.Name()
		entries, err := templateFS.ReadDir(dir + "/" + groupName)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			switch ext {
			case ".yaml", ".yml", ".json":
				base := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
				out = append(out, groupName+"/"+base)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func listClawHomeConfigDir(rel string) ([]string, error) {
	root, err := clawConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, filepath.FromSlash(rel))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		switch ext {
		case ".yaml", ".yml", ".json":
			out = append(out, strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		}
	}
	sort.Strings(out)
	return out, nil
}

func listNestedClawHomeConfigDir(rel string) ([]string, error) {
	root, err := clawConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, filepath.FromSlash(rel))
	groups, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, group := range groups {
		if !group.IsDir() {
			continue
		}
		groupName := group.Name()
		entries, err := os.ReadDir(filepath.Join(dir, groupName))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			switch ext {
			case ".yaml", ".yml", ".json":
				out = append(out, groupName+"/"+strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func mergeSortedNames(groups ...[]string) []string {
	seen := map[string]struct{}{}
	for _, group := range groups {
		for _, name := range group {
			if strings.TrimSpace(name) == "" {
				continue
			}
			seen[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
