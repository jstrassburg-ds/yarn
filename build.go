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
		// 1. BP_YARN_VERSION environment variable (highest priority)
		// 2. Build plan entry metadata (from detect phase or other buildpacks)
		// 3. Default version (lowest priority)
		version := "default"
		if envVersion, exists := os.LookupEnv(YarnVersionEnvVar); exists && envVersion != "" {
			version = envVersion
			logger.Process("Using Yarn version %s from %s environment variable", version, YarnVersionEnvVar)
		} else if planVersion, ok := entry.Metadata["version"].(string); ok && planVersion != "" {
			version = planVersion
			logger.Process("Using Yarn version %s from build plan", version)
		} else {
			logger.Process("Using default Yarn version")
		}

		// Check if this is a Yarn Berry project that should use Corepack
		// Note: We still resolve the dependency to maintain compatibility with existing tests
		// but we handle the installation differently for Berry versions

		// Continue with traditional approach for all versions (including Berry for now)
		dependency, err := dependencyManager.Resolve(
			filepath.Join(context.CNBPath, "buildpack.toml"),
			entry.Name,
			version,
			context.Stack)
		if err != nil {
			return packit.BuildResult{}, err
		}

		// Check if this is a Berry version after resolving the dependency
		if isYarnBerry(dependency.Version) {
			logger.Process("Detected Yarn Berry version %s, using Corepack approach", dependency.Version)
			return handleYarnBerryWithCorepack(context, yarnLayer, dependency, planner, logger, clock, sbomGenerator)
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

// isYarnBerry checks if the version string indicates a Yarn Berry version (2.x, 3.x, 4.x+)
func isYarnBerry(version string) bool {
	if version == "default" {
		return false // Default is typically Yarn Classic 1.x
	}

	// Check if version starts with 2., 3., 4., etc. (but not 1.)
	if strings.HasPrefix(version, "2.") || strings.HasPrefix(version, "3.") || strings.HasPrefix(version, "4.") {
		return true
	}

	// Handle semver ranges like "4.*", "^2.0.0", etc.
	if strings.Contains(version, "2") || strings.Contains(version, "3") || strings.Contains(version, "4") {
		return !strings.HasPrefix(version, "1.")
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
