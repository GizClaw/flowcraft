package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type testFile struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Raid        string   `yaml:"raid"`
	Turns       []string `yaml:"turns"`
}

func testCmd(args []string) error {
	flags := flag.NewFlagSet("test", flag.ExitOnError)
	testSource := flags.String("test", "", "test path or embedded test name")
	timeout := flags.Duration("timeout", 2*time.Minute, "maximum duration per test turn")
	flags.Parse(args)

	if *testSource == "" {
		return fmt.Errorf("test requires -test\n\n%s", usage())
	}
	if *timeout <= 0 {
		return fmt.Errorf("test requires --timeout > 0")
	}
	_, test, err := readTest(*testSource)
	if err != nil {
		return err
	}
	raid := strings.TrimSpace(test.Raid)
	if raid == "" {
		return fmt.Errorf("test %q requires raid", *testSource)
	}
	if len(test.Turns) == 0 {
		return fmt.Errorf("test %q requires at least one turn", *testSource)
	}

	output, err := prepareTestRunOutput(raid, time.Now())
	if err != nil {
		return err
	}
	restoreOutput, err := captureAutoRunOutput(output)
	if err != nil {
		return err
	}
	defer restoreOutput()

	workspacePath := filepath.Join(output.Dir, "workspace")
	if err := WriteConfig(templateFS, raid, workspacePath); err != nil {
		return fmt.Errorf("create test workspace: %w", err)
	}
	metrics, err := runTestTurns(workspacePath, output.ChatLogPath, test.Turns, *timeout)
	if writeErr := writeTestStats(output.StatsPath, metrics); writeErr != nil && err == nil {
		err = writeErr
	}
	if err != nil {
		return err
	}
	restoreOutput()
	restoreOutput = func() {}
	fmt.Printf("wrote test %s\n", output.Dir)
	return nil
}

func testRunDir(raid string, now time.Time) string {
	return filepath.Join(".out", fmt.Sprintf("%s_-_%s", autoRunNamePart(raid), now.Format("20060102_150405")))
}

func prepareTestRunOutput(raid string, now time.Time) (autoRunOutput, error) {
	runDir := testRunDir(raid, now)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return autoRunOutput{}, fmt.Errorf("create test output directory: %w", err)
	}
	return autoRunOutput{
		Dir:         runDir,
		ChatLogPath: filepath.Join(runDir, "chat_log.txt"),
		StatsPath:   filepath.Join(runDir, "stats.txt"),
		StdoutPath:  filepath.Join(runDir, "stdout.txt"),
		StderrPath:  filepath.Join(runDir, "stderr.txt"),
	}, nil
}

func readTest(source string) (string, testFile, error) {
	path, raw, err := readTestSource(source)
	if err != nil {
		return "", testFile{}, err
	}
	var test testFile
	if err := yaml.Unmarshal(raw, &test); err != nil {
		return "", testFile{}, fmt.Errorf("test %q: %w", source, err)
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), test, nil
}

func readTestSource(source string) (string, []byte, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil, fmt.Errorf("test source is required")
	}
	if path, raw, ok, err := readClawHomeTestSource(source); ok {
		return path, raw, err
	}
	if path, ok, err := findEmbeddedTestSource(source); ok {
		if err != nil {
			return "", nil, err
		}
		raw, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return "", nil, err
		}
		return path, raw, nil
	}
	raw, fileErr := os.ReadFile(source)
	if fileErr == nil {
		return source, raw, nil
	}
	return "", nil, fmt.Errorf("test %q: embedded lookup failed; local file: %v", source, fileErr)
}

func readClawHomeTestSource(source string) (string, []byte, bool, error) {
	root, err := clawConfigDir()
	if err != nil {
		return "", nil, true, err
	}
	for _, rel := range clawHomeTestCandidates(source) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err == nil {
			return path, raw, true, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return path, nil, true, err
		}
	}
	return "", nil, false, nil
}

func clawHomeTestCandidates(source string) []string {
	source = strings.TrimPrefix(strings.TrimSpace(source), "./")
	if source == "" || !fs.ValidPath(source) {
		return nil
	}
	switch {
	case strings.HasPrefix(source, "examples/test/"):
		source = strings.TrimPrefix(source, "examples/test/")
	case strings.HasPrefix(source, "examples/tests/"):
		source = strings.TrimPrefix(source, "examples/tests/")
	case strings.HasPrefix(source, "example/test/"):
		source = strings.TrimPrefix(source, "example/test/")
	case strings.HasPrefix(source, "test/"):
		source = strings.TrimPrefix(source, "test/")
	case strings.HasPrefix(source, "tests/"):
		source = strings.TrimPrefix(source, "tests/")
	}
	if strings.Count(source, "/") != 1 {
		return nil
	}
	exts := []string{""}
	if filepath.Ext(source) == "" {
		exts = []string{".yaml", ".yml", ".json"}
	}
	out := make([]string, 0, len(exts))
	for _, ext := range exts {
		out = append(out, "configs/test/"+source+ext)
	}
	return out
}

func findEmbeddedTestSource(source string) (string, bool, error) {
	source = strings.TrimPrefix(strings.TrimSpace(source), "./")
	if source == "" || !fs.ValidPath(source) {
		return "", false, nil
	}
	switch {
	case strings.HasPrefix(source, "examples/test/"):
		return readEmbeddedTestPath(source)
	case strings.HasPrefix(source, "examples/tests/"):
		return readEmbeddedTestPath("examples/test/" + strings.TrimPrefix(source, "examples/tests/"))
	case strings.HasPrefix(source, "example/test/"):
		return readEmbeddedTestPath("examples/test/" + strings.TrimPrefix(source, "example/test/"))
	case strings.HasPrefix(source, "test/"):
		return readEmbeddedTestPath("examples/test/" + strings.TrimPrefix(source, "test/"))
	case strings.HasPrefix(source, "tests/"):
		return readEmbeddedTestPath("examples/test/" + strings.TrimPrefix(source, "tests/"))
	case strings.Count(source, "/") == 1:
		path, err := findTestTemplate(source)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", false, nil
			}
			return "", true, err
		}
		return path, true, nil
	default:
		return "", false, nil
	}
}

func readEmbeddedTestPath(path string) (string, bool, error) {
	found, err := findEmbeddedTestPath(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", true, err
	}
	return found, true, nil
}

func findEmbeddedTestPath(path string) (string, error) {
	if filepath.Ext(path) != "" {
		info, err := fs.Stat(templateFS, path)
		if err == nil && !info.IsDir() {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("test %q: %w", path, fs.ErrNotExist)
	}
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		candidate := path + ext
		info, err := fs.Stat(templateFS, candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("test %q: %w", path, fs.ErrNotExist)
}

func findTestTemplate(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || !fs.ValidPath(name) || strings.Count(name, "/") != 1 {
		return "", fmt.Errorf("invalid test %q", name)
	}
	return findEmbeddedTestPath("examples/test/" + name)
}
