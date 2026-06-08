package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

func helpCmd(args []string) error {
	if len(args) == 0 {
		printHelp()
		return nil
	}
	switch args[0] {
	case "list-examples":
		return listExamplesCmd()
	default:
		return fmt.Errorf("unknown help command %q\n\n%s", args[0], usage())
	}
}

func listExamplesCmd() error {
	raids, err := listRaids()
	if err != nil {
		return fmt.Errorf("list raids: %w", err)
	}
	personas, err := listPersonas()
	if err != nil {
		return fmt.Errorf("list personas: %w", err)
	}
	tests, err := listTests()
	if err != nil {
		return fmt.Errorf("list tests: %w", err)
	}
	fmt.Println("raids:")
	for _, name := range raids {
		fmt.Printf("  %s\n", name)
	}
	fmt.Println("personas:")
	for _, name := range personas {
		fmt.Printf("  %s\n", name)
	}
	fmt.Println("tests:")
	for _, name := range tests {
		fmt.Printf("  %s\n", name)
	}
	return nil
}

func listRaids() ([]string, error) {
	return listExampleDir("examples/raids")
}

func listPersonas() ([]string, error) {
	return listExampleDir("examples/personas")
}

func listTests() ([]string, error) {
	return listNestedExampleDir("examples/tests")
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
