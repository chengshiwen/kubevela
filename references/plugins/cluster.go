/*
Copyright 2021 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package plugins

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commontypes "github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/cue"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/utils/common"
	"github.com/oam-dev/kubevela/pkg/utils/helm"
	util2 "github.com/oam-dev/kubevela/pkg/utils/util"
)

// DescriptionUndefined indicates the description is not defined
const DescriptionUndefined = "description not defined"

// GetCapabilitiesFromCluster will get capability from K8s cluster
func GetCapabilitiesFromCluster(ctx context.Context, namespace string, c common.Args, selector labels.Selector) ([]types.Capability, error) {
	workloads, _, err := GetComponentsFromCluster(ctx, namespace, c, selector)
	if err != nil {
		return nil, err
	}
	traits, _, err := GetTraitsFromCluster(ctx, namespace, c, selector)
	if err != nil {
		return nil, err
	}
	workloads = append(workloads, traits...)
	return workloads, nil
}

// GetComponentsFromCluster will get capability from K8s cluster
func GetComponentsFromCluster(ctx context.Context, namespace string, c common.Args, selector labels.Selector) ([]types.Capability, []error, error) {
	newClient, err := c.GetClient()
	if err != nil {
		return nil, nil, err
	}
	dm, err := discoverymapper.New(c.Config)
	if err != nil {
		return nil, nil, err
	}

	var templates []types.Capability
	var componentsDefs v1beta1.ComponentDefinitionList
	err = newClient.List(ctx, &componentsDefs, &client.ListOptions{Namespace: namespace, LabelSelector: selector})
	if err != nil {
		return nil, nil, fmt.Errorf("list ComponentDefinition err: %w", err)
	}

	var templateErrors []error
	for _, cd := range componentsDefs.Items {
		ref, err := util.ConvertWorkloadGVK2Definition(dm, cd.Spec.Workload.Definition)
		if err != nil {
			templateErrors = append(templateErrors, errors.Wrapf(err, "convert workload definition `%s` failed", cd.Name))
			continue
		}
		tmp, err := HandleDefinition(cd.Name, ref.Name, cd.Annotations, cd.Spec.Extension, types.TypeComponentDefinition, nil, cd.Spec.Schematic)
		if err != nil {
			templateErrors = append(templateErrors, errors.Wrapf(err, "handle workload template `%s` failed", cd.Name))
			continue
		}
		tmp.Namespace = namespace
		if tmp, err = validateCapabilities(tmp, dm, cd.Name, ref); err != nil {
			return nil, nil, err
		}
		templates = append(templates, tmp)
	}
	return templates, templateErrors, nil
}

// GetTraitsFromCluster will get capability from K8s cluster
func GetTraitsFromCluster(ctx context.Context, namespace string, c common.Args, selector labels.Selector) ([]types.Capability, []error, error) {
	newClient, err := c.GetClient()
	if err != nil {
		return nil, nil, err
	}
	dm, err := discoverymapper.New(c.Config)
	if err != nil {
		return nil, nil, err
	}
	var templates []types.Capability
	var traitDefs v1beta1.TraitDefinitionList
	err = newClient.List(ctx, &traitDefs, &client.ListOptions{Namespace: namespace, LabelSelector: selector})
	if err != nil {
		return nil, nil, fmt.Errorf("list TraitDefinition err: %w", err)
	}

	var templateErrors []error
	for _, td := range traitDefs.Items {
		tmp, err := HandleDefinition(td.Name, td.Spec.Reference.Name, td.Annotations, td.Spec.Extension, types.TypeTrait, td.Spec.AppliesToWorkloads, td.Spec.Schematic)
		if err != nil {
			templateErrors = append(templateErrors, errors.Wrapf(err, "handle trait template `%s` failed", td.Name))
			continue
		}
		tmp.Namespace = namespace
		if tmp, err = validateCapabilities(tmp, dm, td.Name, td.Spec.Reference); err != nil {
			return nil, nil, err
		}
		templates = append(templates, tmp)
	}
	return templates, templateErrors, nil
}

// validateCapabilities validates whether helm charts are successful installed, GVK are successfully retrieved.
func validateCapabilities(tmp types.Capability, dm discoverymapper.DiscoveryMapper, definitionName string, reference commontypes.DefinitionReference) (types.Capability, error) {
	var err error
	if tmp.Install != nil {
		tmp.Source = &types.Source{ChartName: tmp.Install.Helm.Name}
		ioStream := util2.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
		if err = helm.InstallHelmChart(ioStream, tmp.Install.Helm); err != nil {
			return tmp, fmt.Errorf("unable to install helm chart dependency %s(%s from %s) for this trait '%s': %w ", tmp.Install.Helm.Name, tmp.Install.Helm.Version, tmp.Install.Helm.URL, definitionName, err)
		}
	}
	gvk, err := util.GetGVKFromDefinition(dm, reference)
	if err != nil {
		errMsg := err.Error()
		var substr = "no matches for "
		if strings.Contains(errMsg, substr) {
			err = fmt.Errorf("expected provider: %s", strings.Split(errMsg, substr)[1])
		}
		return tmp, fmt.Errorf("installing capability '%s'... %w", definitionName, err)
	}
	tmp.CrdInfo = &types.CRDInfo{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
	}

	return tmp, nil
}

// HandleDefinition will handle definition to capability
func HandleDefinition(name, crdName string, annotation map[string]string, extension *runtime.RawExtension, tp types.CapType, applyTo []string, schematic *commontypes.Schematic) (types.Capability, error) {
	var tmp types.Capability
	tmp, err := HandleTemplate(extension, schematic, name)
	if err != nil {
		return types.Capability{}, err
	}
	tmp.Type = tp
	if tp == types.TypeTrait {
		tmp.AppliesTo = applyTo
	}
	tmp.CrdName = crdName
	tmp.Description = GetDescription(annotation)
	return tmp, nil
}

// GetDescription get description from annotation
func GetDescription(annotation map[string]string) string {
	if annotation == nil {
		return DescriptionUndefined
	}
	desc, ok := annotation[types.AnnDescription]
	if !ok {
		return DescriptionUndefined
	}
	desc = strings.ReplaceAll(desc, "\n", " ")
	return desc
}

// HandleTemplate will handle definition template to capability
func HandleTemplate(in *runtime.RawExtension, schematic *commontypes.Schematic, name string) (types.Capability, error) {
	tmp, err := appfile.ConvertTemplateJSON2Object(name, in, schematic)
	if err != nil {
		return types.Capability{}, err
	}
	tmp.Name = name
	// if spec.template is not empty it should has the highest priority
	if schematic != nil && schematic.CUE != nil {
		tmp.CueTemplate = schematic.CUE.Template
		tmp.CueTemplateURI = ""
	}
	if tmp.CueTemplateURI != "" {
		b, err := common.HTTPGet(context.Background(), tmp.CueTemplateURI)
		if err != nil {
			return types.Capability{}, err
		}
		tmp.CueTemplate = string(b)
	}
	if tmp.CueTemplate == "" {
		return types.Capability{}, errors.New("template not exist in definition")
	}
	if err != nil {
		return types.Capability{}, err
	}
	tmp.Parameters, err = cue.GetParameters(tmp.CueTemplate)
	if err != nil {
		return types.Capability{}, err
	}
	return tmp, nil
}

// SyncDefinitionsToLocal sync definitions to local
func SyncDefinitionsToLocal(ctx context.Context, c common.Args, localDefinitionDir string) ([]types.Capability, []string, error) {
	var syncedTemplates []types.Capability
	var warnings []string

	templates, templateErrors, err := GetComponentsFromCluster(ctx, types.DefaultKubeVelaNS, c, nil)
	if err != nil {
		return nil, nil, err
	}
	if len(templateErrors) > 0 {
		for _, e := range templateErrors {
			warnings = append(warnings, fmt.Sprintf("WARN: %v, you will unable to use this component capability\n", e))
		}
	}
	syncedTemplates = append(syncedTemplates, templates...)
	SinkTemp2Local(templates, localDefinitionDir)

	templates, templateErrors, err = GetTraitsFromCluster(ctx, types.DefaultKubeVelaNS, c, nil)
	if err != nil {
		return nil, warnings, err
	}
	if len(templateErrors) > 0 {
		for _, e := range templateErrors {
			warnings = append(warnings, fmt.Sprintf("WARN: %v, you will unable to use this trait capability\n", e))
		}
	}
	syncedTemplates = append(syncedTemplates, templates...)
	SinkTemp2Local(templates, localDefinitionDir)
	return syncedTemplates, warnings, nil
}

// SyncDefinitionToLocal sync definitions to local
func SyncDefinitionToLocal(ctx context.Context, c common.Args, localDefinitionDir string, capabilityName string) (*types.Capability, error) {
	var foundCapability bool

	newClient, err := c.GetClient()
	if err != nil {
		return nil, err
	}
	var componentDef v1beta1.ComponentDefinition
	err = newClient.Get(ctx, client.ObjectKey{Namespace: types.DefaultKubeVelaNS, Name: capabilityName}, &componentDef)
	if err == nil {
		// return nil, fmt.Errorf("get WorkloadDefinition err: %w", err)
		foundCapability = true
	}
	if foundCapability {
		dm, err := c.GetDiscoveryMapper()
		if err != nil {
			return nil, err
		}
		ref, err := util.ConvertWorkloadGVK2Definition(dm, componentDef.Spec.Workload.Definition)
		if err != nil {
			return nil, err
		}
		template, err := HandleDefinition(capabilityName, ref.Name,
			componentDef.Annotations, componentDef.Spec.Extension, types.TypeComponentDefinition, nil, componentDef.Spec.Schematic)
		if err == nil {
			return &template, nil
		}
	}

	foundCapability = false
	var traitDef v1beta1.TraitDefinition
	err = newClient.Get(ctx, client.ObjectKey{Namespace: types.DefaultKubeVelaNS, Name: capabilityName}, &traitDef)
	if err == nil {
		foundCapability = true
	}
	if foundCapability {
		template, err := HandleDefinition(capabilityName, traitDef.Spec.Reference.Name,
			traitDef.Annotations, traitDef.Spec.Extension, types.TypeTrait, nil, traitDef.Spec.Schematic)
		if err == nil {
			return &template, nil
		}
	}
	return nil, fmt.Errorf("%s is not a valid workload type or trait", capabilityName)
}
