package yarn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/paketo-buildpacks/packit/v2"
)

func Detect() packit.DetectFunc {
	return func(context packit.DetectContext) (packit.DetectResult, error) {
		// Version selection priority:
		// 1. packageManager field in package.json (highest priority)
		// 2. BP_YARN_VERSION environment variable
		// 3. No specific version requirement (lowest priority)

		var requirements []packit.BuildPlanRequirement

		// Check for Yarn version in package.json first
		packageJsonVersion, err := getYarnVersionFromPackageJson(context.WorkingDir)
		if err == nil && packageJsonVersion != "" {
			requirements = append(requirements, packit.BuildPlanRequirement{
				Name: YarnDependency,
				Metadata: map[string]interface{}{
					"version": packageJsonVersion,
				},
			})
		} else if version, exists := os.LookupEnv(YarnVersionEnvVar); exists && version != "" {
			// Fallback to environment variable if no package.json version found
			requirements = append(requirements, packit.BuildPlanRequirement{
				Name: YarnDependency,
				Metadata: map[string]interface{}{
					"version": version,
				},
			})
		}

		return packit.DetectResult{
			Plan: packit.BuildPlan{
				Provides: []packit.BuildPlanProvision{
					{Name: YarnDependency},
				},
				Requires: requirements,
			},
		}, nil
	}
}

// PackageJson represents the structure of a package.json file
type PackageJson struct {
	PackageManager string `json:"packageManager"`
}

// getYarnVersionFromPackageJson extracts the Yarn version from package.json packageManager field
func getYarnVersionFromPackageJson(workingDir string) (string, error) {
	packageJsonPath := filepath.Join(workingDir, "package.json")

	// Check if package.json exists
	if _, err := os.Stat(packageJsonPath); os.IsNotExist(err) {
		return "", err
	}

	// Read package.json file
	data, err := os.ReadFile(packageJsonPath)
	if err != nil {
		return "", err
	}

	// Parse JSON
	var pkg PackageJson
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", err
	}

	// Extract Yarn version from packageManager field
	// Expected format: "yarn@4.9.2" or "yarn@^4.0.0"
	if pkg.PackageManager != "" && strings.HasPrefix(pkg.PackageManager, "yarn@") {
		version := strings.TrimPrefix(pkg.PackageManager, "yarn@")
		// Remove semver prefixes like ^ or ~
		version = strings.TrimPrefix(version, "^")
		version = strings.TrimPrefix(version, "~")
		return version, nil
	}

	return "", nil
}
