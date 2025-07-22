package yarn

import (
	"os"

	"github.com/paketo-buildpacks/packit/v2"
)

func Detect() packit.DetectFunc {
	return func(context packit.DetectContext) (packit.DetectResult, error) {
		// Check if a specific yarn version is requested via environment variable
		var requirements []packit.BuildPlanRequirement
		if version, exists := os.LookupEnv(YarnVersionEnvVar); exists && version != "" {
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
