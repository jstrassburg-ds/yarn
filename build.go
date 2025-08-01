package yarn

import (
	"fmt"	
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/paketo-buildpacks/packit/v2"
	"github.com/paketo-buildpacks/packit/v2/chronos"
	"github.com/paketo-buildpacks/packit/v2/draft"
	"github.com/paketo-buildpacks/packit/v2/postal"
	"github.com/paketo-buildpacks/packit/v2/sbom"
	"github.com/paketo-buildpacks/packit/v2/scribe"
)

//go:generate faux --interface DependencyManager --output fakes/dependency_manager.go
type DependencyManager interface {
	Resolve(path, id, version, stack string) (postal.Dependency, error)
	Deliver(dependency postal.Dependency, cnbPath, layerPath, platformPath string) error
	GenerateBillOfMaterials(dependencies ...postal.Dependency) []packit.BOMEntry
}

//go:generate faux --interface SBOMGenerator --output fakes/sbom_generator.go
type SBOMGenerator interface {
	GenerateFromDependency(dependency postal.Dependency, dir string) (sbom.SBOM, error)
}

func Build(
	dependencyManager DependencyManager,
	sbomGenerator SBOMGenerator,
	clock chronos.Clock,
	logger scribe.Emitter,
) packit.BuildFunc {
	return func(context packit.BuildContext) (packit.BuildResult, error) {
		logger.Title("%s %s", context.BuildpackInfo.Name, context.BuildpackInfo.Version)

		yarnLayer, err := context.Layers.Get(YarnLayerName)
		if err != nil {
			return packit.BuildResult{}, err
		}

		planner := draft.NewPlanner()
		entry, _ := planner.Resolve("yarn", context.Plan.Entries, nil)

		// Version selection priority:
		// 1. Build plan entry metadata (from detect phase package.json parsing - highest priority)
		// 2. BP_YARN_VERSION environment variable
		// 3. Default version (lowest priority)
		version := "default"
		if planVersion, ok := entry.Metadata["version"].(string); ok && planVersion != "" {
			version = planVersion
			logger.Process("Using Yarn version %s from build plan (package.json)", version)
		} else if envVersion, exists := os.LookupEnv(YarnVersionEnvVar); exists && envVersion != "" {
			version = envVersion
			logger.Process("Using Yarn version %s from %s environment variable", version, YarnVersionEnvVar)
		} else {
			logger.Process("Using default Yarn version")
		}

		// Check if this is a Yarn Berry project that should use Corepack
		// Handle Berry versions directly with Corepack instead of dependency resolution
		if IsYarnBerry(version) {
			logger.Process("Detected Yarn Berry version %s, using Corepack approach", version)
			// Create a mock dependency for Berry versions since they're managed by Corepack
			dependency := postal.Dependency{
				ID:      "yarn",
				Name:    "Yarn",
				Version: version,
				// Use a deterministic checksum based on version for caching
				Checksum: fmt.Sprintf("sha256:corepack-yarn-%s", version),
			}
			return handleYarnBerryWithCorepack(context, yarnLayer, dependency, planner, logger, clock, sbomGenerator)
		}

		// Continue with traditional buildpack dependency resolution for Yarn Classic (1.x) versions
		dependency, err := dependencyManager.Resolve(
			filepath.Join(context.CNBPath, "buildpack.toml"),
			entry.Name,
			version,
			context.Stack)
		if err != nil {
			return packit.BuildResult{}, err
		}

		bom := dependencyManager.GenerateBillOfMaterials(dependency)

		launch, build := planner.MergeLayerTypes("yarn", context.Plan.Entries)

		var buildMetadata = packit.BuildMetadata{}
		var launchMetadata = packit.LaunchMetadata{}
		if build {
			buildMetadata = packit.BuildMetadata{BOM: bom}
		}

		if launch {
			launchMetadata = packit.LaunchMetadata{BOM: bom}
		}

		cachedSHA, ok := yarnLayer.Metadata[DependencyCacheKey].(string)
		if ok && postal.Checksum(dependency.Checksum).MatchString(cachedSHA) {
			logger.Process("Reusing cached layer %s", yarnLayer.Path)
			logger.Break()

			yarnLayer.Launch, yarnLayer.Build, yarnLayer.Cache = launch, build, build

			return packit.BuildResult{
				Layers: []packit.Layer{yarnLayer},
				Build:  buildMetadata,
				Launch: launchMetadata,
			}, nil
		}

		logger.Process("Executing build process")

		yarnLayer, err = yarnLayer.Reset()
		if err != nil {
			return packit.BuildResult{}, err
		}

		yarnLayer.Launch, yarnLayer.Build, yarnLayer.Cache = launch, build, build

		logger.Subprocess("Installing Yarn")

		duration, err := clock.Measure(func() error {
			return dependencyManager.Deliver(dependency, context.CNBPath, yarnLayer.Path, context.Platform.Path)
		})
		if err != nil {
			return packit.BuildResult{}, err
		}
		logger.Action("Completed in %s", duration.Round(time.Millisecond))
		logger.Break()

		sbomDisabled, err := checkSbomDisabled()
		if err != nil {
			return packit.BuildResult{}, err
		}

		if sbomDisabled {
			logger.Subprocess("Skipping SBOM generation for Yarn")
			logger.Break()
		} else {
			logger.GeneratingSBOM(yarnLayer.Path)
			var sbomContent sbom.SBOM
			duration, err = clock.Measure(func() error {
				sbomContent, err = sbomGenerator.GenerateFromDependency(dependency, yarnLayer.Path)
				return err
			})
			if err != nil {
				return packit.BuildResult{}, err
			}

			logger.Action("Completed in %s", duration.Round(time.Millisecond))
			logger.Break()

			logger.FormattingSBOM(context.BuildpackInfo.SBOMFormats...)
			yarnLayer.SBOM, err = sbomContent.InFormats(context.BuildpackInfo.SBOMFormats...)
			if err != nil {
				return packit.BuildResult{}, err
			}
		}

		yarnLayer.Metadata = map[string]interface{}{
			DependencyCacheKey: dependency.Checksum,
		}

		return packit.BuildResult{
			Layers: []packit.Layer{yarnLayer},
			Build:  buildMetadata,
			Launch: launchMetadata,
		}, nil
	}
}

func checkSbomDisabled() (bool, error) {
	if disableStr, ok := os.LookupEnv("BP_DISABLE_SBOM"); ok {
		disable, err := strconv.ParseBool(disableStr)
		if err != nil {
			return false, fmt.Errorf("failed to parse BP_DISABLE_SBOM value %s: %w", disableStr, err)
		}
		return disable, nil
	}
	return false, nil
}

// IsYarnBerry checks if the version string indicates a Yarn Berry version (2.x, 3.x, 4.x+)
func IsYarnBerry(version string) bool {
	if version == "default" {
		return false // Default is typically Yarn Classic 1.x
	}

	// Check if version starts with 2., 3., 4., 5., etc. (but not 1.)
	if strings.HasPrefix(version, "2.") || strings.HasPrefix(version, "3.") ||
		strings.HasPrefix(version, "4.") || strings.HasPrefix(version, "5.") ||
		strings.HasPrefix(version, "6.") || strings.HasPrefix(version, "7.") ||
		strings.HasPrefix(version, "8.") || strings.HasPrefix(version, "9.") {
		return true
	}

	// Handle semver ranges like "4.*", "^2.0.0", etc.
	// If it contains any digit >= 2 and doesn't start with 1., it's likely Berry
	for _, char := range []string{"2", "3", "4", "5", "6", "7", "8", "9"} {
		if strings.Contains(version, char) && !strings.HasPrefix(version, "1.") {
			return true
		}
	}

	return false
}

// handleYarnBerryWithCorepack handles installation for Yarn Berry versions using Corepack
func handleYarnBerryWithCorepack(context packit.BuildContext, yarnLayer packit.Layer, dependency postal.Dependency, planner draft.Planner, logger scribe.Emitter, clock chronos.Clock, sbomGenerator SBOMGenerator) (packit.BuildResult, error) {
	launch, build := planner.MergeLayerTypes("yarn", context.Plan.Entries)

	// Check if we have a cached installation
	cachedSHA, ok := yarnLayer.Metadata[DependencyCacheKey].(string)
	if ok && postal.Checksum(dependency.Checksum).MatchString(cachedSHA) {
		logger.Process("Reusing cached Yarn Berry layer %s", yarnLayer.Path)
		logger.Break()

		yarnLayer.Launch, yarnLayer.Build, yarnLayer.Cache = launch, build, build

		// No BOM needed for cached layer - SBOM will be restored from layer
		return packit.BuildResult{
			Layers: []packit.Layer{yarnLayer},
		}, nil
	}

	logger.Process("Setting up Yarn Berry via Corepack")

	yarnLayer, err := yarnLayer.Reset()
	if err != nil {
		return packit.BuildResult{}, err
	}

	yarnLayer.Launch, yarnLayer.Build, yarnLayer.Cache = launch, build, build

	// Enable Corepack
	logger.Subprocess("Enabling Corepack")
	duration, err := clock.Measure(func() error {
		return enableCorepack(context.WorkingDir, logger)
	})
	if err != nil {
		return packit.BuildResult{}, err
	}
	logger.Action("Completed in %s", duration.Round(time.Millisecond))

	// Set and cache the Yarn version
	logger.Subprocess("Setting Yarn version %s", dependency.Version)
	duration, err = clock.Measure(func() error {
		return setYarnVersion(context.WorkingDir, dependency.Version, logger)
	})
	if err != nil {
		return packit.BuildResult{}, err
	}
	logger.Action("Completed in %s", duration.Round(time.Millisecond))
	logger.Break()

	// Set up environment variables for Corepack
	yarnLayer.SharedEnv.Default("COREPACK_ENABLE_STRICT", "0")
	yarnLayer.LaunchEnv.Default("COREPACK_ENABLE_STRICT", "0")
	yarnLayer.BuildEnv.Default("COREPACK_ENABLE_STRICT", "0")

	// Generate SBOM for Yarn Berry installation
	sbomDisabled, err := checkSbomDisabled()
	if err != nil {
		return packit.BuildResult{}, err
	}

	if !sbomDisabled {
		logger.GeneratingSBOM(yarnLayer.Path)
		var sbomContent sbom.SBOM
		duration, err = clock.Measure(func() error {
			// Use the provided sbomGenerator to generate SBOM from the dependency
			sbomContent, err = sbomGenerator.GenerateFromDependency(dependency, yarnLayer.Path)
			return err
		})
		if err != nil {
			return packit.BuildResult{}, err
		}

		logger.Action("Completed in %s", duration.Round(time.Millisecond))
		logger.Break()

		logger.FormattingSBOM(context.BuildpackInfo.SBOMFormats...)
		yarnLayer.SBOM, err = sbomContent.InFormats(context.BuildpackInfo.SBOMFormats...)
		if err != nil {
			return packit.BuildResult{}, err
		}
	}

	// Cache the dependency checksum
	yarnLayer.Metadata = map[string]interface{}{
		DependencyCacheKey: dependency.Checksum,
	}

	return packit.BuildResult{
		Layers: []packit.Layer{yarnLayer},
	}, nil
}

// enableCorepack enables Corepack to manage package manager versions
func enableCorepack(workingDir string, logger scribe.Emitter) error {
	cmd := exec.Command("corepack", "enable")
	cmd.Dir = workingDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Subprocess("Failed to enable Corepack: %s", string(output))
		return fmt.Errorf("failed to enable corepack: %w", err)
	}

	logger.Detail("Corepack enabled successfully")
	return nil
}

// setYarnVersion sets the Yarn version using Corepack, which downloads and caches the binary
func setYarnVersion(workingDir, version string, logger scribe.Emitter) error {
	// Use 'yarn set version' to download and cache the specific version
	cmd := exec.Command("yarn", "set", "version", version)
	cmd.Dir = workingDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Subprocess("Failed to set Yarn version %s: %s", version, string(output))
		return fmt.Errorf("failed to set yarn version %s: %w", version, err)
	}

	logger.Detail("Yarn version %s set and cached successfully", version)
	return nil
}
