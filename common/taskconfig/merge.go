package taskconfig

import (
	"reflect"

	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"code.uber.internal/infra/peloton/util"

	"github.com/gogo/protobuf/proto"
)

// Merge returns the merged task config between a base and an override. The
// merge will only happen of top-level fields, not recursively.
// If any of the arguments is nil, no merge will happen, and the non-nil
// argument (if exists) is returned.
func Merge(base *task.TaskConfig, override *task.TaskConfig) *task.TaskConfig {
	if override == nil {
		return base
	}

	if base == nil {
		return override
	}

	merged := &task.TaskConfig{}

	baseVal := reflect.ValueOf(*base)
	overrideVal := reflect.ValueOf(*override)
	mergedVal := reflect.ValueOf(merged).Elem()
	for i := 0; i < baseVal.NumField(); i++ {
		field := overrideVal.Field(i)

		switch field.Kind() {
		case reflect.Bool:
			// override bool
			mergedVal.Field(i).Set(overrideVal.Field(i))
		case reflect.String:
			if field.String() == "" {
				// set to base config value if the string is empty
				mergedVal.Field(i).Set(baseVal.Field(i))
			} else {
				// merged config should have the overridden value
				mergedVal.Field(i).Set(overrideVal.Field(i))
			}
		default:
			if field.IsNil() {
				// set to base config value if the value is empty
				mergedVal.Field(i).Set(baseVal.Field(i))
			} else {
				// merged config should have the overridden value
				mergedVal.Field(i).Set(overrideVal.Field(i))
			}
		}
	}
	return merged
}

// RetainBaseSecretsInInstanceConfig ensures that instance config retains all
// secrets from default config. We store secrets as secret volumes at the
// default config level for the job as part of container info.
// This works if instance config does not override the container info.
// However in some cases there is a use case for this override (ex: controller
// job and executor job use different images). In case where instance config
// overrides container info, the "merge" will blindly override the containerinfo
// in default config. So the instance which has container info in the instance
// config will never get secrets. This function ensures that even if the
// instance config overrides container info, it will still retain secrets if any
// from the default config.
func RetainBaseSecretsInInstanceConfig(defaultConfig *task.TaskConfig,
	instanceConfig *task.TaskConfig) *task.TaskConfig {
	// if default config doesn't have secrets, just return
	if defaultConfig == nil ||
		!util.ConfigHasSecretVolumes(defaultConfig) {
		return instanceConfig
	}
	clonedDefaultConfig := proto.Clone(defaultConfig).(*task.TaskConfig)
	secretVolumes := util.RemoveSecretVolumesFromConfig(
		clonedDefaultConfig)
	if instanceConfig.GetContainer().GetVolumes() != nil {
		for _, secretVolume := range secretVolumes {
			instanceConfig.GetContainer().Volumes = append(
				instanceConfig.GetContainer().Volumes, secretVolume)
		}
		return instanceConfig
	}
	instanceConfig.GetContainer().Volumes = secretVolumes
	return instanceConfig
}
