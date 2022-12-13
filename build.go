package pnpm

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

		pnpmLayer, err := context.Layers.Get(PnpmLayerName)
		if err != nil {
			return packit.BuildResult{}, err
		}

		planner := draft.NewPlanner()
		entry, _ := planner.Resolve("pnpm", context.Plan.Entries, nil)
		version, ok := entry.Metadata["version"].(string)
		if !ok {
			version = "default"
		}

		dependency, err := dependencyManager.Resolve(
			filepath.Join(context.CNBPath, "buildpack.toml"),
			entry.Name,
			version,
			context.Stack)
		if err != nil {
			return packit.BuildResult{}, err
		}

		bom := dependencyManager.GenerateBillOfMaterials(dependency)

		launch, build := planner.MergeLayerTypes("pnpm", context.Plan.Entries)

		var buildMetadata = packit.BuildMetadata{}
		var launchMetadata = packit.LaunchMetadata{}
		if build {
			buildMetadata = packit.BuildMetadata{BOM: bom}
		}

		if launch {
			launchMetadata = packit.LaunchMetadata{BOM: bom}
		}

		cachedSHA, ok := pnpmLayer.Metadata[DependencyCacheKey].(string)
		if ok && cachedSHA == dependency.SHA256 {
			logger.Process("Reusing cached layer %s", pnpmLayer.Path)
			logger.Break()

			pnpmLayer.Launch, pnpmLayer.Build, pnpmLayer.Cache = launch, build, build

			return packit.BuildResult{
				Layers: []packit.Layer{pnpmLayer},
				Build:  buildMetadata,
				Launch: launchMetadata,
			}, nil
		}

		logger.Process("Executing build process")

		pnpmLayer, err = pnpmLayer.Reset()
		if err != nil {
			return packit.BuildResult{}, err
		}

		pnpmLayer.Launch, pnpmLayer.Build, pnpmLayer.Cache = launch, build, build

		logger.Subprocess("Installing Pnpm")

		duration, err := clock.Measure(func() error {
			return dependencyManager.Deliver(dependency, context.CNBPath, pnpmLayer.Path, context.Platform.Path)
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
			logger.Subprocess("Skipping SBOM generation for Pnpm")
			logger.Break()
		} else {
			logger.GeneratingSBOM(pnpmLayer.Path)
			var sbomContent sbom.SBOM
			duration, err = clock.Measure(func() error {
				sbomContent, err = sbomGenerator.GenerateFromDependency(dependency, pnpmLayer.Path)
				return err
			})
			if err != nil {
				return packit.BuildResult{}, err
			}

			logger.Action("Completed in %s", duration.Round(time.Millisecond))
			logger.Break()

			logger.FormattingSBOM(context.BuildpackInfo.SBOMFormats...)
			pnpmLayer.SBOM, err = sbomContent.InFormats(context.BuildpackInfo.SBOMFormats...)
			if err != nil {
				return packit.BuildResult{}, err
			}
		}

		pnpmLayer.Metadata = map[string]interface{}{
			DependencyCacheKey: dependency.SHA256,
		}

		return packit.BuildResult{
			Layers: []packit.Layer{pnpmLayer},
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
