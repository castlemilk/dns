// Command collect-go-licenses copies exact legal files for the modules linked
// into a Go command and writes a deterministic inventory beside them.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var legalFilePattern = regexp.MustCompile(`(?i)^(license|licence|copying|notice|copyright|patents|authors|contributors)([._-].*)?$`)

type module struct {
	Path    string  `json:"Path"`
	Version string  `json:"Version"`
	Dir     string  `json:"Dir"`
	Main    bool    `json:"Main"`
	Replace *module `json:"Replace"`
}

type packageDescription struct {
	Module *module `json:"Module"`
}

type evidence struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type inventoryModule struct {
	Path     string     `json:"path"`
	Version  string     `json:"version"`
	GoMod    string     `json:"goMod"`
	Evidence []evidence `json:"evidence"`
}

type inventory struct {
	SchemaVersion  int               `json:"schemaVersion"`
	ProjectLicense string            `json:"projectLicense"`
	Modules        []inventoryModule `json:"modules"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	output := flag.String("out", "", "output directory")
	projectLicense := flag.String("project-license", "LICENSE", "project license file")
	target := flag.String("target", "./cmd/dns", "Go command to inspect")
	flag.Parse()
	if *output == "" {
		return errors.New("-out is required")
	}

	modules, err := runtimeModules(*target)
	if err != nil {
		return err
	}
	if len(modules) == 0 {
		return errors.New("no runtime modules found")
	}

	if err := os.RemoveAll(*output); err != nil {
		return fmt.Errorf("clear output: %w", err)
	}
	if err := os.MkdirAll(*output, 0o755); err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	if err := copyExact(*projectLicense, filepath.Join(*output, "LICENSE")); err != nil {
		return fmt.Errorf("copy project license: %w", err)
	}

	result := inventory{SchemaVersion: 1, ProjectLicense: "LICENSE"}
	for _, dependency := range modules {
		source := dependency
		if dependency.Replace != nil {
			source = dependency.Replace
		}
		if source.Dir == "" {
			return fmt.Errorf("module %s@%s has no source directory", dependency.Path, dependency.Version)
		}

		version := dependency.Version
		if version == "" {
			version = "local"
		}
		destination, err := safeModuleDirectory(*output, dependency.Path, version)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return fmt.Errorf("create module directory for %s: %w", dependency.Path, err)
		}

		goModSource := filepath.Join(source.Dir, "go.mod")
		goModDestination := filepath.Join(destination, "go.mod")
		if err := copyExact(goModSource, goModDestination); err != nil {
			return fmt.Errorf("copy go.mod for %s: %w", dependency.Path, err)
		}

		legalFiles, err := moduleLegalFiles(source.Dir)
		if err != nil {
			return fmt.Errorf("inspect legal files for %s: %w", dependency.Path, err)
		}
		if len(legalFiles) == 0 {
			return fmt.Errorf("module %s@%s has no license evidence", dependency.Path, dependency.Version)
		}

		entry := inventoryModule{
			Path:    dependency.Path,
			Version: dependency.Version,
			GoMod:   filepath.ToSlash(filepath.Join("third_party", "go", dependency.Path+"@"+version, "go.mod")),
		}
		for _, legalFile := range legalFiles {
			destinationFile := filepath.Join(destination, filepath.Base(legalFile))
			contents, err := os.ReadFile(legalFile)
			if err != nil {
				return fmt.Errorf("read %s: %w", legalFile, err)
			}
			if err := os.WriteFile(destinationFile, contents, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", destinationFile, err)
			}
			digest := sha256.Sum256(contents)
			entry.Evidence = append(entry.Evidence, evidence{
				File:   filepath.ToSlash(filepath.Join("third_party", "go", dependency.Path+"@"+version, filepath.Base(legalFile))),
				SHA256: hex.EncodeToString(digest[:]),
			})
		}
		result.Modules = append(result.Modules, entry)
	}

	manifest, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode inventory: %w", err)
	}
	manifest = append(manifest, '\n')
	if err := os.WriteFile(filepath.Join(*output, "THIRD_PARTY.json"), manifest, 0o644); err != nil {
		return fmt.Errorf("write inventory: %w", err)
	}
	fmt.Printf("collected exact license evidence for %d runtime Go modules\n", len(result.Modules))
	return nil
}

func runtimeModules(target string) ([]*module, error) {
	command := exec.Command("go", "list", "-deps", "-json", target)
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, fmt.Errorf("go list failed: %s", strings.TrimSpace(string(exitError.Stderr)))
		}
		return nil, fmt.Errorf("run go list: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(output))
	unique := make(map[string]*module)
	for {
		var description packageDescription
		if err := decoder.Decode(&description); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if description.Module == nil || description.Module.Main {
			continue
		}
		key := description.Module.Path + "@" + description.Module.Version
		unique[key] = description.Module
	}

	result := make([]*module, 0, len(unique))
	for _, dependency := range unique {
		result = append(result, dependency)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Path == result[right].Path {
			return result[left].Version < result[right].Version
		}
		return result[left].Path < result[right].Path
	})
	return result, nil
}

func moduleLegalFiles(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && legalFilePattern.MatchString(entry.Name()) {
			result = append(result, filepath.Join(directory, entry.Name()))
		}
	}
	sort.Strings(result)
	return result, nil
}

func safeModuleDirectory(output, modulePath, version string) (string, error) {
	if modulePath == "" || filepath.IsAbs(modulePath) || strings.Contains(modulePath, "\\") {
		return "", fmt.Errorf("unsafe module path %q", modulePath)
	}
	for _, component := range strings.Split(modulePath, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("unsafe module path %q", modulePath)
		}
	}
	if version == "" || strings.ContainsAny(version, `/\\`) || version == "." || version == ".." {
		return "", fmt.Errorf("unsafe module version %q", version)
	}
	root, err := filepath.Abs(filepath.Join(output, "third_party", "go"))
	if err != nil {
		return "", err
	}
	destination, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(modulePath)+"@"+version))
	if err != nil {
		return "", err
	}
	if destination == root || !strings.HasPrefix(destination, root+string(filepath.Separator)) {
		return "", fmt.Errorf("module destination escapes output: %q", destination)
	}
	return destination, nil
}

func copyExact(source, destination string) error {
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destination, contents, 0o644)
}
